# Terraform / OpenTofu provider for Staysa Cloud

Manage projects, applications, managed databases, custom domains, hosted DNS and object
storage on a Staysa Cloud platform as code. Works with Terraform 1.0+ and OpenTofu.

```hcl
provider "staysa" {} # STAYSA_ENDPOINT and STAYSA_TOKEN from the environment
```

See `examples/main.tf` and the platform documentation (`/docs/terraform` on your console)
for every resource, attribute and import ID.

## Build and try it locally

```
go build -o terraform-provider-staysa .
```

Point Terraform or OpenTofu at the folder holding the binary with a dev override in
`~/.terraformrc` (OpenTofu: `~/.tofurc`; Windows: `%APPDATA%\terraform.rc` / `tofu.rc`):

```hcl
provider_installation {
  dev_overrides {
    "gattoufirabi/staysa" = "/path/to/terraform-provider-staysa"
  }
  direct {}
}
```

With a dev override, skip `init` and run `plan` / `apply` directly.

## Release

Tag `vX.Y.Z` and run `goreleaser release --clean` with `GPG_FINGERPRINT` set. The
registries need the repository to be named `terraform-provider-staysa` and the public
GPG key registered with them.
