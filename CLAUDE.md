# pfrest — Agent Operating Guide

> **⛔ NO DIRECT APPLIES TO ANY DEVICE — EVER.**
>
> Direct changes to **any** device — router, firewall, switch, access point, hypervisor, mail gateway, or any other appliance — are **NEVER** permitted, by anyone, for any reason. This bans hand-run `tofu apply`, hand-run `ansible-playbook`, SSH/serial/CLI config writes, REST/API mutations, and web-GUI/console edits.
>
> **Every change MUST flow through the sanctioned pipeline:** declare intent in **prod-netbox** (the single source of truth), then realize it **only** through **prod-semaphore** (the sanctioned runner). A change that did not go **prod-netbox → prod-semaphore** must never reach a device.
>
> **Sole exception:** a specific direct action is permitted *only* when the operator authorizes that exact action in advance by answering an explicit, **alarm-flavored `AskUserQuestion`** — one that names the device, the precise action, and the risk — **in the affirmative**. No standing grants, no inferred permission, no carrying one approval to another action or device. Absent that in-the-moment "yes," the answer is no.
>
> **Never offload the work onto the operator.** When you are blocked, ask for the break-glass authorization that lets *you* do the job — never ask the operator to run a command, SSH in, or make the change on your behalf. The operator grants permission; they do not perform your labour.

Native OpenTofu/Terraform provider for **pfSense** via the **REST API v2**
(`pfSense-pkg-RESTAPI`, https://pfrest.org). Sibling of `../tofu-aruba-aos` and
`../openwrt-ubus` (same generic-over-the-API philosophy, same toolchain). The
workspace-root `../CLAUDE.md` applies; this adds specifics.

## What this is / isn't

- **Is:** a provider for pfSense driven entirely through the documented
  `/api/v2` REST surface, authed with a stateless `X-API-Key` header.
- **Isn't:** a config.xml / SSH / GUI-scraping provider. Everything goes through
  the REST API package.

## Design tenets

- **The generic resources here are `pfrest_object` (+ data source)** — they
  address any `/api/v2` endpoint. Resist adding typed resources until there's a
  real ergonomics need.
- **The subset plan modifier is `subsetMatches`**; `body` is the keys we manage.
  State holds the full device object; declared keys match -> 0-diff.
- **Two endpoint shapes** (verify against docs / live box, don't assume):
  - **Collection** (`singleton = false`, default): POST creates and the server
    assigns `id` (returned in `data.id`); GET/PATCH/DELETE address by `?id=<id>`.
    PATCH also carries `id` in the body.
  - **Singleton** (`singleton = true`, e.g. `system/dns`): GET reads, PATCH
    updates in place, no create/delete. ForceNew on `singleton`.
- **Response envelope** is `{code,status,response_id,message,data,...}`; the
  client unwraps `data` and the resource works on that.

## Toolchain

- Go 1.26.4 (`/home/jameson/.local/go`), `terraform-plugin-framework` v1.19.0.
  **Do not add or bump dependencies** — reuse `../tofu-aruba-aos`'s vetted set.
- Provider address: `registry.terraform.io/JamesonRGrieve/pfrest`.
- `make check` (tidy+fmt+vet+test+build) is the gate; the `.githooks/pre-commit`
  re-runs it. Never `--no-verify`.

## Hard rules

- **No secrets in the repo.** The `api_key` comes from the provider config
  (OpenBao -> `TF_VAR_*` via Semaphore).
- **NEVER touch `omg-pfsense`** (the production CLIENT firewall at
  192.168.255.129 / 192.168.1.1). Live validation is **lab-only**
  (`pfsense-lab`, NetBox device 21, VM 92003 on pve-gigabyte, 100.64.92.x).
- Drive any change against a managed target via Semaphore, plan-first / 0-diff.
