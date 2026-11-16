// Command platformctl is the command-line client of the platform's REST API.
//
//	export PLATFORM_URL=https://console.example.com
//	export PLATFORM_TOKEN=pla_...        # created in the console, organization > API
//
//	platformctl whoami
//	platformctl projects
//	platformctl services <project>
//	platformctl status <project>/<service>
//	platformctl deploy <project>/<service> [--image IMAGE] [--wait]
//	platformctl logs <project>/<service> [--filter TEXT]
//	platformctl secret set <project>/<service> NAME     (value read from standard input)
//	platformctl var set <project> NAME [--plain]         (shared by every service of the project)
//	platformctl preview create|list|delete <project>/<service> [NAME] ...
//
// Standard library only: one static binary, easy to drop into any CI image.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type client struct {
	base, token string
	hc          *http.Client
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		usage(0)
	}
	// RFC 6761: names under .localhost always mean this machine. Browsers and curl do
	// this; Go's resolver on some systems does not, so do it here (local dev stack).
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(strings.ToLower(host), ".localhost") {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	c := &client{
		base:  strings.TrimRight(os.Getenv("PLATFORM_URL"), "/"),
		token: os.Getenv("PLATFORM_TOKEN"),
		hc:    &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
	if c.base == "" || c.token == "" {
		fail("set PLATFORM_URL (the console address) and PLATFORM_TOKEN (an API token from the console)")
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "whoami":
		err = c.whoami()
	case "projects":
		err = c.projects()
	case "services":
		if len(args) != 1 {
			usage(2)
		}
		err = c.services(args[0])
	case "status":
		err = c.status(target(args))
	case "deploy":
		err = c.deploy(args)
	case "logs":
		err = c.logs(args)
	case "secret":
		if len(args) != 3 || args[0] != "set" {
			usage(2)
		}
		err = c.setSecret(args[1], args[2])
	case "var":
		err = c.vars(args)
	case "job":
		err = c.jobs(args)
	case "scale":
		if len(args) < 2 {
			usage(2)
		}
		err = c.scale(args)
	case "redeploy":
		if len(args) != 1 {
			usage(2)
		}
		err = c.redeploy(args[0])
	case "preview":
		err = c.preview(args)
	case "build":
		err = c.build(args)
	case "bucket":
		err = c.buckets(args)
	case "key":
		err = c.storageKeys(args)
	default:
		usage(2)
	}
	if err != nil {
		fail(err.Error())
	}
}

func usage(code int) {
	fmt.Fprintln(os.Stderr, `usage: platformctl <command>

  whoami                                   show the token's organization and role
  projects                                 list projects
  services <project>                       list services of a project
  status <project>/<service>               settings and recent deployments
  deploy <project>/<service> [--image IMAGE] [--wait]
                                           deploy current settings, optionally with a new image
  logs <project>/<service> [--filter TEXT] last log lines (6 hours, up to 300)
  secret set <project>/<service> NAME      set a secret; the value is read from standard input
  var list <project>                       shared project variables (secret values hidden)
  var set <project> NAME [--plain]         set a shared variable for every service of the project;
                                           value from standard input; secret unless --plain
  var unset <project> NAME                 delete a shared variable
  redeploy <project>                       redeploy every running service of the project
  scale <project>/<service> N [--wait]     run N instances (1 to 10) behind the proxy, with no downtime
  job list <project>/<service>             scheduled jobs and their last run
  job run <project>/<service> JOB [--wait] run a job now; --wait prints its log and exit status
  job logs <project>/<service> JOB         log of the job's last run
  preview create <project>/<service> NAME [--image IMAGE] [--ttl HOURS] [--wait]
                                           create or update a preview environment (e.g. pr-42)
  preview list <project>/<service>         list previews
  preview delete <project>/<service> NAME  delete a preview
  build <project>/<service> [--no-deploy] [--wait]
                                           build the service's git branch (and deploy it unless --no-deploy)
  bucket list                              S3 buckets with usage, endpoint and region
  bucket create NAME [--quota MB]          create bucket <org>-NAME (default quota 1024 MB)
  bucket delete NAME                       delete a bucket and all its objects
  key list                                 S3 access keys and the buckets they reach
  key create NAME BUCKET=read|write...     create an access key; prints its secret once
  key delete ID|ACCESS_KEY_ID              delete an access key

environment: PLATFORM_URL (console address), PLATFORM_TOKEN (API token "pla_...")`)
	os.Exit(code)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "platformctl:", msg)
	os.Exit(1)
}

// target splits "project/service".
func target(args []string) (string, string) {
	if len(args) < 1 {
		usage(2)
	}
	p, s, ok := strings.Cut(args[0], "/")
	if !ok || p == "" || s == "" {
		fail("expected <project>/<service>, for example shop/web")
	}
	return url.PathEscape(p), url.PathEscape(s)
}

func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func table(header string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, header)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

func (c *client) whoami() error {
	var me map[string]string
	if err := c.do("GET", "/api/v1/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("organization %s (%s)\nrole         %s\ntoken        %s\n", me["organization"], me["name"], me["role"], me["token"])
	return nil
}

func (c *client) projects() error {
	var ps []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Services int    `json:"services"`
	}
	if err := c.do("GET", "/api/v1/projects", nil, &ps); err != nil {
		return err
	}
	var rows [][]string
	for _, p := range ps {
		rows = append(rows, []string{p.ID, p.Name, fmt.Sprint(p.Services)})
	}
	table("PROJECT\tNAME\tSERVICES", rows)
	return nil
}

func (c *client) services(project string) error {
	var ss []struct {
		ID, Image, Status string
	}
	if err := c.do("GET", "/api/v1/projects/"+url.PathEscape(project)+"/services", nil, &ss); err != nil {
		return err
	}
	var rows [][]string
	for _, s := range ss {
		rows = append(rows, []string{s.ID, s.Status, s.Image})
	}
	table("SERVICE\tSTATUS\tIMAGE", rows)
	return nil
}

type deployment struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	Image    string `json:"image"`
	By       string `json:"by"`
}

func (c *client) status(p, s string) error {
	var sv struct {
		ID          string       `json:"id"`
		Image       string       `json:"image"`
		Port        int          `json:"port"`
		CPU         int          `json:"cpu_millis"`
		Memory      int          `json:"memory_mb"`
		URL         string       `json:"url"`
		Deployments []deployment `json:"deployments"`
	}
	if err := c.do("GET", "/api/v1/projects/"+p+"/services/"+s, nil, &sv); err != nil {
		return err
	}
	fmt.Printf("service  %s\nurl      %s\nimage    %s\nport     %d · %d millicores · %d MB\n\n", sv.ID, sv.URL, sv.Image, sv.Port, sv.CPU, sv.Memory)
	var rows [][]string
	for _, d := range sv.Deployments {
		rows = append(rows, []string{fmt.Sprint(d.Revision), d.Status, d.Image, d.By})
	}
	table("REV\tSTATUS\tIMAGE\tBY", rows)
	return nil
}

func (c *client) deploy(args []string) error {
	p, s := target(args)
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	image := fs.String("image", "", "image to deploy (default: the current image)")
	wait := fs.Bool("wait", false, "wait until the deployment is live or failed")
	_ = fs.Parse(args[1:])
	var res struct {
		Revision  int    `json:"revision"`
		StatusURL string `json:"status_url"`
	}
	var body any
	if *image != "" {
		body = map[string]string{"image": *image}
	}
	if err := c.do("POST", "/api/v1/projects/"+p+"/services/"+s+"/deploy", body, &res); err != nil {
		return err
	}
	fmt.Printf("revision %d queued\n", res.Revision)
	if !*wait {
		return nil
	}
	return c.waitDeployment(res.StatusURL)
}

// waitDeployment follows a deployment until it is live (nil) or failed (error).
func (c *client) waitDeployment(statusURL string) error {
	last := ""
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		var d deployment
		if err := c.do("GET", statusURL, nil, &d); err != nil {
			return err
		}
		if line := d.Status + " " + firstLine(d.Message); line != last && d.Status != "live" && d.Status != "failed" {
			fmt.Println(strings.TrimSpace(line))
			last = line
		}
		switch d.Status {
		case "live":
			fmt.Printf("revision %d is live\n", d.Revision)
			return nil
		case "failed":
			return fmt.Errorf("revision %d failed: %s", d.Revision, d.Message)
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("timed out waiting for the deployment")
}

func (c *client) logs(args []string) error {
	p, s := target(args)
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	filter := fs.String("filter", "", "only lines containing this text")
	_ = fs.Parse(args[1:])
	path := "/api/v1/projects/" + p + "/services/" + s + "/logs"
	if *filter != "" {
		path += "?q=" + url.QueryEscape(*filter)
	}
	var lines []struct{ Time, Msg, Stream string }
	if err := c.do("GET", path, nil, &lines); err != nil {
		return err
	}
	for _, l := range lines {
		ts := l.Time
		if len(ts) > 19 {
			ts = strings.Replace(ts[:19], "T", " ", 1)
		}
		fmt.Printf("%s  %s\n", ts, l.Msg)
	}
	return nil
}

func readValue() (string, error) {
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "value (end with Enter): ")
	}
	value, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(value, "\r\n"), nil
}

func (c *client) vars(args []string) error {
	if len(args) < 2 {
		usage(2)
	}
	p := url.PathEscape(args[1])
	switch args[0] {
	case "list":
		var vs []struct {
			Name, Value string
			Secret      bool
		}
		if err := c.do("GET", "/api/v1/projects/"+p+"/variables", nil, &vs); err != nil {
			return err
		}
		var rows [][]string
		for _, v := range vs {
			val := v.Value
			if v.Secret {
				val = "(secret)"
			}
			rows = append(rows, []string{v.Name, val})
		}
		table("NAME\tVALUE", rows)
		return nil
	case "set":
		if len(args) < 3 || len(args) > 4 || (len(args) == 4 && args[3] != "--plain") {
			usage(2)
		}
		value, err := readValue()
		if err != nil {
			return err
		}
		secret := len(args) == 3
		if err := c.do("PUT", "/api/v1/projects/"+p+"/variables/"+url.PathEscape(args[2]), map[string]any{"value": value, "secret": secret}, nil); err != nil {
			return err
		}
		fmt.Printf("variable %s saved; run \"platformctl redeploy %s\" to apply it\n", args[2], args[1])
		return nil
	case "unset":
		if len(args) != 3 {
			usage(2)
		}
		if err := c.do("DELETE", "/api/v1/projects/"+p+"/variables/"+url.PathEscape(args[2]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("variable %s deleted; run \"platformctl redeploy %s\" to apply it\n", args[2], args[1])
		return nil
	}
	usage(2)
	return nil
}

type jobRun struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Log      string `json:"log"`
	Started  string `json:"started_at"`
}

func (c *client) jobs(args []string) error {
	if len(args) < 2 {
		usage(2)
	}
	p, s := target(args[1:2])
	base := "/api/v1/projects/" + p + "/services/" + s + "/jobs"
	switch args[0] {
	case "list":
		var js []struct {
			ID, Schedule, Command string
			Enabled               bool
			NextRun               *string `json:"next_run_at"`
			LastStatusJSON        string  `json:"last_status"`
		}
		if err := c.do("GET", base, nil, &js); err != nil {
			return err
		}
		var rows [][]string
		for _, j := range js {
			next := "-"
			if j.NextRun != nil && j.Enabled {
				next = *j.NextRun
			}
			state := j.LastStatusJSON
			if state == "" {
				state = "never run"
			}
			if !j.Enabled {
				state += " (paused)"
			}
			rows = append(rows, []string{j.ID, j.Schedule, state, next, j.Command})
		}
		table("JOB\tSCHEDULE\tLAST RUN\tNEXT RUN (UTC)\tCOMMAND", rows)
		return nil
	case "run", "logs":
		if len(args) < 3 {
			usage(2)
		}
		job := url.PathEscape(args[2])
		latest := func() (*jobRun, error) {
			var runs []jobRun
			if err := c.do("GET", base+"/"+job+"/runs", nil, &runs); err != nil {
				return nil, err
			}
			if len(runs) == 0 {
				return nil, nil
			}
			var full jobRun
			err := c.do("GET", base+"/"+job+"/runs/"+runs[0].ID, nil, &full)
			return &full, err
		}
		if args[0] == "logs" {
			r, err := latest()
			if err != nil {
				return err
			}
			if r == nil {
				return errors.New("this job has not run yet")
			}
			fmt.Println(r.Log)
			fmt.Printf("status: %s\n", r.Status)
			return nil
		}
		before, err := latest()
		if err != nil {
			return err
		}
		if err := c.do("POST", base+"/"+job+"/run", nil, nil); err != nil {
			return err
		}
		if len(args) < 4 || args[3] != "--wait" {
			fmt.Printf("job %s requested\n", args[2])
			return nil
		}
		deadline := time.Now().Add(65 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
			r, err := latest()
			if err != nil {
				return err
			}
			if r == nil || (before != nil && r.ID == before.ID) || r.Status == "running" {
				continue
			}
			fmt.Println(r.Log)
			if r.Status != "succeeded" {
				return fmt.Errorf("job %s %s", args[2], strings.ReplaceAll(r.Status, "_", " "))
			}
			fmt.Printf("job %s succeeded\n", args[2])
			return nil
		}
		return errors.New("gave up waiting for the job")
	}
	usage(2)
	return nil
}

func (c *client) scale(args []string) error {
	p, s := target(args[:1])
	var n int
	if _, err := fmt.Sscan(args[1], &n); err != nil {
		return errors.New("the number of instances must be a number from 1 to 10")
	}
	var out struct {
		Revision  int    `json:"revision"`
		StatusURL string `json:"status_url"`
	}
	if err := c.do("POST", "/api/v1/projects/"+p+"/services/"+s+"/scale", map[string]int{"instances": n}, &out); err != nil {
		return err
	}
	fmt.Printf("scaling %s to %d instance(s): revision %d queued\n", args[0], n, out.Revision)
	if len(args) > 2 && args[2] == "--wait" {
		return c.waitDeployment(out.StatusURL)
	}
	return nil
}

func (c *client) redeploy(project string) error {
	var out struct {
		Redeploying int `json:"redeploying"`
	}
	if err := c.do("POST", "/api/v1/projects/"+url.PathEscape(project)+"/redeploy", nil, &out); err != nil {
		return err
	}
	fmt.Printf("redeploying %d service(s)\n", out.Redeploying)
	return nil
}

func (c *client) setSecret(t, name string) error {
	p, s := target([]string{t})
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "value (end with Enter): ")
	}
	value, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	value = strings.TrimRight(value, "\r\n")
	if err := c.do("PUT", "/api/v1/projects/"+p+"/services/"+s+"/secrets/"+url.PathEscape(name), map[string]string{"value": value}, nil); err != nil {
		return err
	}
	fmt.Printf("secret %s saved; deploy %s/%s to apply it\n", name, p, s)
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------- preview environments ----------

type preview struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Image    string    `json:"image"`
	Status   string    `json:"status"`
	Expires  time.Time `json:"expires_at"`
	Revision int       `json:"revision"`
}

func (c *client) preview(args []string) error {
	if len(args) < 2 {
		usage(2)
	}
	sub := args[0]
	p, s := target(args[1:])
	base := "/api/v1/projects/" + p + "/services/" + s + "/previews"
	switch sub {
	case "list":
		var list []preview
		if err := c.do("GET", base, nil, &list); err != nil {
			return err
		}
		var rows [][]string
		for _, v := range list {
			rows = append(rows, []string{v.Name, v.Status, v.URL, v.Expires.UTC().Format("2006-01-02 15:04 UTC"), v.Image})
		}
		table("NAME\tSTATUS\tURL\tEXPIRES\tIMAGE", rows)
		return nil
	case "delete":
		if len(args) != 3 {
			usage(2)
		}
		if err := c.do("DELETE", base+"/"+url.PathEscape(args[2]), nil, nil); err != nil {
			return err
		}
		fmt.Println("preview", args[2], "deleted")
		return nil
	case "create":
		if len(args) < 3 {
			usage(2)
		}
		name := args[2]
		fs := flag.NewFlagSet("preview create", flag.ExitOnError)
		image := fs.String("image", "", "image (default: the service's image)")
		ttl := fs.Int("ttl", 0, "lifetime in hours (default 72, at most 720)")
		wait := fs.Bool("wait", false, "wait until the preview is live or failed")
		_ = fs.Parse(args[3:])
		var res preview
		if err := c.do("POST", base, map[string]any{"name": name, "image": *image, "ttl_hours": *ttl}, &res); err != nil {
			return err
		}
		fmt.Println("preview", res.Name, "deploying at", res.URL)
		if !*wait {
			return nil
		}
		deadline := time.Now().Add(15 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			var list []preview
			if err := c.do("GET", base, nil, &list); err != nil {
				return err
			}
			for _, v := range list {
				if v.Name != name {
					continue
				}
				switch v.Status {
				case "live":
					fmt.Println("preview", name, "is live at", v.URL)
					return nil
				case "failed":
					return fmt.Errorf("preview %s failed to deploy (see its logs in the console)", name)
				}
			}
		}
		return errors.New("timed out waiting for the preview")
	}
	usage(2)
	return nil
}

// ---------- builds from git ----------

func (c *client) build(args []string) error {
	p, s := target(args)
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	noDeploy := fs.Bool("no-deploy", false, "only build and push the image")
	wait := fs.Bool("wait", false, "wait until the build finishes and print its log")
	_ = fs.Parse(args[1:])
	var res struct {
		Number    int    `json:"number"`
		StatusURL string `json:"status_url"`
	}
	if err := c.do("POST", "/api/v1/projects/"+p+"/services/"+s+"/builds", map[string]bool{"deploy": !*noDeploy}, &res); err != nil {
		return err
	}
	fmt.Printf("build #%d queued\n", res.Number)
	if !*wait {
		return nil
	}
	deadline := time.Now().Add(40 * time.Minute)
	printed := 0
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		var b struct {
			Status string `json:"status"`
			Image  string `json:"image"`
			Error  string `json:"error"`
			Log    string `json:"log"`
		}
		if err := c.do("GET", res.StatusURL, nil, &b); err != nil {
			return err
		}
		if len(b.Log) > printed {
			fmt.Print(b.Log[printed:])
			printed = len(b.Log)
		}
		switch b.Status {
		case "done":
			fmt.Printf("build #%d done: %s\n", res.Number, b.Image)
			return nil
		case "failed":
			return fmt.Errorf("build #%d failed: %s", res.Number, b.Error)
		}
	}
	return errors.New("timed out waiting for the build")
}

func (c *client) buckets(args []string) error {
	if len(args) < 1 {
		usage(2)
	}
	switch args[0] {
	case "list":
		var bs []struct {
			Name, Endpoint, Region string
			S3Name                 string `json:"s3_name"`
			QuotaMB                int    `json:"quota_mb"`
			Objects, Bytes         int64
			Website                string `json:"website_url"`
		}
		if err := c.do("GET", "/api/v1/buckets", nil, &bs); err != nil {
			return err
		}
		var rows [][]string
		for _, b := range bs {
			rows = append(rows, []string{b.Name, b.S3Name, fmt.Sprint(b.Objects), fmt.Sprintf("%.1f/%d MB", float64(b.Bytes)/(1<<20), b.QuotaMB), b.Website})
		}
		table("NAME\tS3 NAME\tOBJECTS\tUSED\tWEBSITE", rows)
		if len(bs) > 0 {
			fmt.Printf("\nendpoint %s  region %s\n", bs[0].Endpoint, bs[0].Region)
		}
		return nil
	case "create":
		if len(args) != 2 && !(len(args) == 4 && args[2] == "--quota") {
			usage(2)
		}
		body := map[string]any{"name": args[1]}
		if len(args) == 4 {
			var q int
			if _, err := fmt.Sscan(args[3], &q); err != nil {
				return errors.New("--quota takes a number of MB")
			}
			body["quota_mb"] = q
		}
		var b struct {
			S3Name   string `json:"s3_name"`
			QuotaMB  int    `json:"quota_mb"`
			Endpoint string
		}
		if err := c.do("POST", "/api/v1/buckets", body, &b); err != nil {
			return err
		}
		fmt.Printf("bucket %s created (%d MB) at %s\n", b.S3Name, b.QuotaMB, b.Endpoint)
		return nil
	case "delete":
		if len(args) != 2 {
			usage(2)
		}
		if err := c.do("DELETE", "/api/v1/buckets/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("bucket %s deleted; its objects are being removed\n", args[1])
		return nil
	}
	usage(2)
	return nil
}

func (c *client) storageKeys(args []string) error {
	if len(args) < 1 {
		usage(2)
	}
	switch args[0] {
	case "list":
		var ks []struct {
			ID, Name, Service string
			AccessKeyID       string            `json:"access_key_id"`
			Buckets           map[string]string `json:"buckets"`
		}
		if err := c.do("GET", "/api/v1/storage-keys", nil, &ks); err != nil {
			return err
		}
		var rows [][]string
		for _, k := range ks {
			var parts []string
			for b, p := range k.Buckets {
				parts = append(parts, b+"="+p)
			}
			rows = append(rows, []string{k.AccessKeyID, k.Name, strings.Join(parts, " "), k.Service})
		}
		table("ACCESS KEY ID\tNAME\tBUCKETS\tSERVICE", rows)
		return nil
	case "create":
		if len(args) < 3 {
			usage(2)
		}
		perms := map[string]string{}
		for _, a := range args[2:] {
			b, p, ok := strings.Cut(a, "=")
			if !ok || (p != "read" && p != "write") {
				return fmt.Errorf("%q: give BUCKET=read or BUCKET=write", a)
			}
			perms[b] = p
		}
		var k struct {
			AccessKeyID string `json:"access_key_id"`
			Secret      string `json:"secret_access_key"`
		}
		if err := c.do("POST", "/api/v1/storage-keys", map[string]any{"name": args[1], "buckets": perms}, &k); err != nil {
			return err
		}
		fmt.Printf("AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n# the secret is not shown again\n", k.AccessKeyID, k.Secret)
		return nil
	case "delete":
		if len(args) != 2 {
			usage(2)
		}
		if err := c.do("DELETE", "/api/v1/storage-keys/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Println("access key deleted")
		return nil
	}
	usage(2)
	return nil
}
