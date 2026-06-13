<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# terraform-provider-pfrest

A native OpenTofu/Terraform provider for **pfSense** via the **REST API v2**
served by the [`pfSense-pkg-RESTAPI`](https://pfrest.org) package
(`pfrest/pfSense-pkg-RESTAPI`). Transport is plain HTTPS with a stateless
`X-API-Key` header — no login/session step.

## Why generic

The pfSense REST API exposes a broad, stable surface under `/api/v2/...`:
collections where the server assigns an `id` on create (`firewall/alias`,
`firewall/rule`, `interface/vlan`, `routing/gateway`, …) and singletons updated
in place (`system/dns`, `system/hostname`, `system/tunable`, …). Rather than
hand-code a resource per feature (and chase package additions forever), this
provider is **generic over the API** — one resource and one data source address
*any* endpoint. That is **100% feature coverage** by construction.

## Resources

### `pfrest_object` (resource)

CRUD + `ImportState` for any addressable pfSense REST resource.

```hcl
# Collection item — POST creates, server assigns id; GET/PATCH/DELETE use ?id=
resource "pfrest_object" "lab_hosts" {
  endpoint = "firewall/alias"
  body = jsonencode({
    name  = "lab_hosts"
    type  = "host"
    descr = "managed by tofu"
    address = ["10.0.0.10", "10.0.0.11"]
  })
}

# Singleton — PATCHed in place, no create/delete
resource "pfrest_object" "dns" {
  endpoint  = "system/dns"
  singleton = true
  body      = jsonencode({ dnsserver = ["1.1.1.1", "8.8.8.8"] })
}
```

**Manage-declared-only / 0-diff imports.** `body` declares *only* the keys you
manage. State holds the full device object; a plan modifier suppresses the diff
when every declared key already matches the device, so:

- importing an existing resource (`tofu import` / `import {}` block) lands at
  **0-diff** with no apply against the firewall, and
- the provider never clobbers device fields you didn't declare.

| Attribute | | Meaning |
|-----------|---|---------|
| `endpoint` | required, ForceNew | the `/api/v2` path (leading slash optional), e.g. `firewall/alias`, `system/dns` |
| `body` | required | JSON object of the keys you manage |
| `singleton` | optional, ForceNew | `true` for in-place PATCH endpoints (no create/delete); default `false` (collection) |
| `object_id` | computed | the pfSense-assigned `id` for a collection item |
| `id` | computed | `<endpoint>` (singleton) or `<endpoint>\|<object_id>` (collection) |

### `pfrest_object` (data source)

```hcl
data "pfrest_object" "aliases" { endpoint = "firewall/aliases" }       # plural list -> .response is a JSON array
data "pfrest_object" "alias3"  { endpoint = "firewall/alias"  object_id = "3" }  # single item via ?id=
data "pfrest_object" "dns"     { endpoint = "system/dns" }             # singleton
```

## Import ids

```sh
tofu import pfrest_object.dns        'system/dns'        # singleton
tofu import pfrest_object.lab_hosts  'firewall/alias|3'  # collection item id 3
```

## Provider configuration

```hcl
terraform {
  required_providers {
    pfrest = { source = "registry.terraform.io/jamesonrgrieve/pfrest" }
  }
}

provider "pfrest" {
  host     = "192.168.7.10"        # no scheme
  api_key  = var.pfsense_api_key   # sensitive; sent as X-API-Key
  insecure = true                  # pfSense self-signed cert (default true)
}
```

Generate an API key in **System > REST API > Keys**, or `POST /api/v2/auth/key`.

## Local build / dev install

```sh
make build          # -> terraform-provider-pfrest
make install        # installs to $DEV_BIN_DIR for a dev_overrides .tfrc
make check          # tidy + fmt + vet + test + build (pre-commit / CI gate)
```

For runners without registry access, install into a filesystem mirror:
`<plugins>/registry.terraform.io/JamesonRGrieve/pfrest/<ver>/<os>_<arch>/terraform-provider-pfrest`
and point a `.terraformrc` `provider_installation { filesystem_mirror {...} }` at it.

## License

AGPL-3.0-or-later.
