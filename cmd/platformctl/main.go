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
	last := ""
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		var d deployment
		if err := c.do("GET", res.StatusURL, nil, &d); err != nil {
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
