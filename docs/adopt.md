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
(AWS, Azure), at the cost of fetching whole objects for those types. Resource names avoid addresses that the states or the root
modules' configuration already use (`web` becomes `web_2`), so an import
never lands on someone else's resource block. Import blocks already in a root
module (an earlier `adopt` not applied yet, also into child modules) keep
their addresses, and objects they target are not written again: same type,
same provider configuration (`provider = vault.other` is not the default
one), the same literal `id` or `identity`, and, for objects listed by a root
module, a block in that root. Objects found by `-scan` or a plugin have no
root, so a block in any root with their provider configuration counts. With several roots or plugins the blocks are grouped under
`# Found by <source>` comments: each group belongs in its own root module.
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

[Back to the README](../README.md) · [All guides](README.md)
