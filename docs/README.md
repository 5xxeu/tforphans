# Guides

- [How it works](how-it-works.md): listing, reading states, matching by resource identity or attributes, `-refresh`
- [Output, exit status and coverage](usage.md): the four groups of objects, failure causes, the coverage line, progress
- [JSON output](json.md): fields, coverage rows, `schema_version`
- [Config file](config.md): `.tforphans.yaml`, `-config`, what a file may set
- [Roots, modules and states](roots-and-states.md): child modules, `-state-roots`, `-state`, workspaces, Terragrunt
- [Regions, zones and other list arguments](list-arguments.md): `-arg`, `zone=auto`, your own list blocks, `-types`
- [Ignoring known resources](ignore.md): the ignore file, default and opt-in presets, ignore hints
- [Bringing orphans under Terraform](adopt.md): `tforphans adopt`
- [Providers without list resources](scan.md): `-scan cloudflare`, `-scan proxmox`
- [Plugins](plugins.md): the plugin format, Vault, Junos
- [In CI](ci.md): a scheduled job with `-strict`
- [Security](security.md): what runs, what is written, what is sent
- [Limitations](limitations.md)

[Back to the README](../README.md)
