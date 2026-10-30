# In CI

A scheduled job that fails when something is unmanaged or could not be
checked, with the JSON report as an artifact (GitLab):

```yaml
tforphans:
  stage: audit
  rules:
    - if: $CI_PIPELINE_SOURCE == "schedule"
  script:
    - terraform -chdir=infra init -input=false
    - tforphans -config audits/aws.yaml -strict -format json > tforphans.json
  artifacts:
    when: always
    paths: [tforphans.json]
```

Exit status `2` means unmanaged objects, `1` an error or, with `-strict`,
incomplete coverage. Products a project does not use go into
`-exclude-types` (not listed) or `-strict-ignore` (listed, but their
failures do not fail the job). `-expect` names the accounts or projects
the job must reach, so that one dropped from the configuration fails it
too (see [usage](usage.md)). `jq '.orphans'` and `jq '.summary.coverage[] |
select(.status != "complete")'` show what to look at. Run it on a schedule
or on the default branch, with read-only credentials: commands
(`-plugin`, `-terraform`) and state URLs come only from the job's command
line, never from a config file a merge request could change.

[Back to the README](../README.md) · [All guides](README.md)
