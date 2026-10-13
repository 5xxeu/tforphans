# Security

- tforphans only reads. It runs `terraform providers schema`, `state pull`
  (only for workspaces `workspace list` printed as list lines),
  `workspace list`, `query`, and `apply -refresh-only` on a temporary root
  that holds nothing but data sources; it never applies your
  configuration, and plans it only with `-refresh` (`plan -refresh-only
  -lock=false`, which writes no state; data sources of your configuration
  are read then, as in any plan). `TF_CLI_ARGS` and `TF_CLI_ARGS_<command>` are not passed
  on (they could redirect state), and the temporary root ignores
  `TF_DATA_DIR` and `TF_WORKSPACE`. Run it with read-only credentials (AWS `ReadOnlyAccess`,
  GCP Viewer, Azure Reader, a Cloudflare "Read all resources" token, a
  Proxmox PVEAuditor token): it needs nothing more.
- It sends nothing anywhere itself except `-state` URLs, which get
  `TF_HTTP_USERNAME`/`TF_HTTP_PASSWORD`, only over https and only on the
  original host (a redirect to another https host, such as a presigned
  storage URL, is followed without them and without a Referer; a redirect
  from https to http is refused). There is no
  telemetry. State contents are kept in memory and never printed. Each
  state read is limited to 1 GiB by default, including state pulls and
  refresh JSON; `-max-state-size` sets a positive byte count or a size such
  as `512M` or `2G` (powers of 1024). An automatic `.tforphans.yaml` may
  lower this limit; raising it requires the command line or `-config FILE`.
- Files it writes: a temporary `zz_tforphans_<pid>.tfquery.hcl` (and
  `zz_tforphans_<pid>_override.tf` when a provider is declared only in child
  modules, `zz_tforphans_<pid>_auto.tf` for automatic list arguments,
  `zz_tforphans_<pid>_version_override.tf` with
  `-ignore-required-version`)
  next to your configuration, removed after the run, and a temporary
  directory for data source scans, removed as well. A run killed with
  SIGKILL can leave them behind; the next run removes the files of a
  process that no longer exists only when their names have a known suffix,
  they are regular files, and their first line starts with
  `# tforphans temporary file, run `. Each file's marker includes a run ID
  generated once per process. Symlinks and unmarked files are left alone.
  A marked file belonging to a live process prevents another run from
  starting in the same root. Adding `zz_tforphans_*` to `.gitignore`
  keeps them out of commits in any case.
- `-state-roots` and `-init` run terraform in every root module they find,
  with your environment. Point them only at repositories you would run
  terraform in anyway: a backend block decides where `TF_HTTP_*` and cloud
  credentials go, and `terraform init` downloads and runs providers.
- `-plugin` commands run with `sh -c` and your environment. Reports and
  errors name plugins by position (`plugin1`, `plugin2`), never by the
  command line, which may carry tokens. `-state` URLs are printed without
  user info and query; user info in a URL counts as credentials, so it is
  sent only over https.
- The ignore file is read from the working directory. In a repository you do
  not control, check it (or pass `-ignore /dev/null`), since it can hide
  findings.
- Names from APIs are printed without control characters, and import blocks
  only contain valid type names and quoted values.

[Back to the README](../README.md) · [All guides](README.md)
