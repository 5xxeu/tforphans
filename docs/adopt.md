# Bringing Orphans under Terraform: `tforphans adopt`

`adopt` takes the same flags and finds the same resources, but writes an
import block for each one instead of a report:

```sh
tforphans adopt -o imports.tf          # or to stdout without -o
terraform plan -generate-config-out=generated.tf
```

Import blocks use resource identity (Terraform 1.12+) and name the provider
when its local name is not the type prefix; objects found by your own list
blocks keep that block's provider (an alias such as `aws.other_account`). `-o` never overwrites an
existing file. Resource names come from the object's own name (its
display name, which for some AWS list resources is just the ID);
`-names-from-tags` uses the `Name` tag instead where the type has tags
(AWS, Azure), at the cost of fetching whole objects for those types (a list
block of your own needs `include_resource = true` for that; a note says so). Resource names avoid addresses that the states or the root
modules' configuration already use (`web` becomes `web_2`), so an import
never lands on someone else's resource block. Import blocks already in a root
module (an earlier `adopt` not applied yet, also into child modules) keep
their addresses, and objects they target are not written again: same type,
same provider configuration (`provider = vault.other` is not the default
one), the same literal `id` or `identity`, and, for objects listed by a root
module, a block in that root. Objects found by `-scan` or a plugin have no
root, so a block in any root with their provider configuration counts
(with `-adopt-root`, only a block in that root). With several roots or plugins the blocks are grouped under
`# Found by <source>` comments: each group belongs in its own root module.
`-out-dir DIR` splits them for you (see below).
Narrow down what to adopt with `-types` or the ignore file, and review
`generated.tf` before applying.

The file starts with a comment saying how many states it was checked
against, how many uncertain objects were left out and which types were not
fully listed (objects of those types may be missing). Import blocks of
scoped objects say where the object lives (`# in zone example.com`). When a
state could not be read, `adopt` writes nothing: its objects would show as
unmanaged and be imported a second time. Fix the access, or pass
`-allow-incomplete`.

Terraform before 1.16 generates resources without their `provider` and
accepts `provider` in an import block only while the configuration is still
to be generated: after `-generate-config-out`, move the `provider` line of
each import block into its generated resource before applying.

Generated configuration holds what the provider reads back from the API.
Some resources need more than that: write-only or secret arguments
(passwords, API tokens), content the API does not return (the code of a
`cloudflare_workers_script`, files uploaded once), or arguments that only
shape creation. `terraform plan` after the import shows them as changes or
missing values; fill them in by hand before applying.

`adopt` exits 0 whether or not it found anything (1 on errors, and with
`-strict` on incomplete coverage); the plain command is the one for CI
checks.

## A file per root module: `-out-dir`

```sh
tforphans adopt -out-dir imports -scan cloudflare -adopt-root cloudflare=zones/example.com zones/*
```

`-out-dir` writes a new directory with one `imports.tf` per root module that
found something, at the root's path from the current directory
(`imports/zones/example.com/imports.tf`). Copy each file into its root module
and run `terraform plan -generate-config-out=generated.tf` there. Nothing is
written into the root modules themselves; the directory must not exist yet,
and it appears whole or not at all. Every root module must be under the
current directory.

Objects found by `-scan` or a plugin have no root module: the provider read
them with the credentials of its own environment, which may point at
another account than any root. They go to `_unassigned/<name>.tf`
(`_unassigned/cloudflare.tf`, `_unassigned/plugin1.tf`) with a note. When you
know where they belong, name the root module with `-adopt-root NAME=DIR`:
`cloudflare=zones/example.com` for `-scan cloudflare`, `plugin1=vault` for
the first `-plugin`. Their blocks then go to that root's file under
`# Found by` comments, their names avoid the addresses of that root's
configuration, and a `-scan` provider is named as that root declares it (a
note says when it does not). An object that several sources found goes
where any of them points (a plugin assigned to a root counts even when
dedupe kept another plugin's copy, and the block then names that plugin's
provider configuration); when they point to different root
modules, it is written once and a note names them. `-out-dir` and `-adopt-root` are accepted only on the command
line.

[Back to the README](../README.md) · [All guides](README.md)
