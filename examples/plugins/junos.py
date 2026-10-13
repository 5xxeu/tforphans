#!/usr/bin/env python3
"""tforphans plugin for Junos devices (provider jeremmfr/junos).

The provider has no list resources and few plural data sources, so this
plugin reads the device configuration as JSON and prints the configured
objects of some resource types as JSON lines:

  junos_interface_physical, junos_interface_logical, junos_bgp_group,
  junos_bgp_neighbor, junos_policyoptions_policy_statement,
  junos_policyoptions_prefix_list, junos_security_zone, junos_application,
  junos_application_set

Usage (one device per run, with the states of that device):

  tforphans -no-query -state-roots junos/fw1 \\
    -plugin 'python3 examples/plugins/junos.py --ssh admin@fw1'
  tforphans -no-query -state-roots junos/fw1 \\
    -plugin 'python3 examples/plugins/junos.py fw1.json'

--ssh runs "show configuration | display json" over ssh (keys from your ssh
agent or config); a file argument is that command's output saved before,
"-" reads it from stdin.

Junos state does not record the device, so every object is "unverified":
it matches state only as uncertain unless -plugin-cluster-states says that
the states given belong to this device.

Only names leave this script: the configuration holds secrets ($9$ and
hashed passwords) and is never printed or written anywhere.

Ownership: a physical interface whose stanza holds only units is left to
junos_interface_logical (it would otherwise show up as an unmanaged
junos_interface_physical next to every managed unit). Objects configured
through groups (apply-groups) are not in the configuration as shown and are
not listed.
"""

import json
import subprocess
import sys

SOURCE = "registry.terraform.io/jeremmfr/junos"
DEFAULT_INSTANCE = "default"  # the provider's name for the main instance


class Shape(Exception):
    """The input is JSON but not a configuration as Junos prints it."""


OUT = []  # printed only when the whole configuration was read


def emit(rtype, identity, import_id, name):
    OUT.append(json.dumps({
        "type": rtype,
        "identity": identity,
        "display_name": name,
        "import_id": import_id,
        "provider_source": SOURCE,
        "unverified": True,
    }, sort_keys=True))


def fail(msg):
    print(json.dumps({"error": msg}))


def section(node, key):
    """node[key] as an object; absent is empty, any other type (null too)
    an error."""
    if key not in node:
        return {}
    v = node[key]
    if not isinstance(v, dict):
        raise Shape(key)
    return v


def items(node, key):
    """Named list elements of node[key]; Junos JSON uses lists, but one
    element may come as an object. Absent is empty, null an error."""
    if key not in node:
        return []
    v = node[key]
    if isinstance(v, dict):
        v = [v]
    if not isinstance(v, list) or not all(isinstance(e, dict) and "name" in e for e in v):
        raise Shape(key)
    return v


def name_of(e):
    n = e["name"]
    # Names may come as {"data": "..."} in some releases.
    if isinstance(n, dict):
        n = n.get("data")
    if not isinstance(n, (str, int)) or str(n) == "":
        raise Shape("name")
    return str(n)


def bgp(protocols, instance):
    for g in items(section(protocols, "bgp"), "group"):
        group = name_of(g)
        emit("junos_bgp_group", {"name": group, "routing_instance": instance},
             f"{group}_-_{instance}", f"{group} ({instance})")
        for n in items(g, "neighbor"):
            ip = name_of(n)
            emit("junos_bgp_neighbor", {"ip": ip, "routing_instance": instance, "group": group},
                 f"{ip}_-_{instance}_-_{group}", f"{ip} in {group} ({instance})")


def scan(conf):
    for i in items(section(conf, "interfaces"), "interface"):
        ifname = name_of(i)
        own = [k for k in i if k not in ("name", "unit") and not k.startswith("@")]
        if own:
            emit("junos_interface_physical", {"name": ifname}, ifname, ifname)
        for u in items(i, "unit"):
            unit = f"{ifname}.{name_of(u)}"
            emit("junos_interface_logical", {"name": unit}, unit, unit)

    bgp(section(conf, "protocols"), DEFAULT_INSTANCE)
    for ri in items(section(conf, "routing-instances"), "instance"):
        bgp(section(ri, "protocols"), name_of(ri))

    po = section(conf, "policy-options")
    for p in items(po, "policy-statement"):
        n = name_of(p)
        emit("junos_policyoptions_policy_statement", {"name": n}, n, n)
    for p in items(po, "prefix-list"):
        n = name_of(p)
        emit("junos_policyoptions_prefix_list", {"name": n}, n, n)

    for z in items(section(section(conf, "security"), "zones"), "security-zone"):
        n = name_of(z)
        emit("junos_security_zone", {"name": n}, n, n)

    apps = section(conf, "applications")
    for a in items(apps, "application"):
        n = name_of(a)
        emit("junos_application", {"name": n}, n, n)
    for a in items(apps, "application-set"):
        n = name_of(a)
        emit("junos_application_set", {"name": n}, n, n)


def read(args):
    if len(args) == 2 and args[0] == "--ssh":
        out = subprocess.run(
            ["ssh", "-o", "BatchMode=yes", "--", args[1], "show configuration | display json | no-more"],
            capture_output=True, text=True, timeout=300)
        if out.returncode != 0:
            # ssh's own message, not the configuration.
            raise RuntimeError("ssh: " + (out.stderr.strip().splitlines() or ["exit %d" % out.returncode])[-1])
        return out.stdout
    if len(args) == 1:
        if args[0] == "-":
            return sys.stdin.read()
        with open(args[0], encoding="utf-8") as f:
            return f.read()
    raise RuntimeError("usage: junos.py FILE | - | --ssh [USER@]HOST")


if len(sys.argv) == 1:
    # A mistake in the command, not an unreachable device: fail loudly.
    sys.exit("usage: junos.py FILE | - | --ssh [USER@]HOST")


def main():
    try:
        data = json.loads(read(sys.argv[1:]))
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as e:
        # An error line, not a failed command: the run goes on with the
        # other plugins and reports this device as not checked. Never echo
        # the input: it may be the configuration.
        if isinstance(e, ValueError):
            fail("junos: the configuration is not JSON (show configuration | display json)")
        else:
            fail(f"junos: cannot read the configuration: {type(e).__name__}: {e}")
        return 0
    # "show configuration | display json" always wraps the configuration;
    # anything else (an rpc-error, another command) is not one.
    conf = data.get("configuration") if isinstance(data, dict) else None
    if not isinstance(conf, dict):
        fail("junos: no configuration object in the input (show configuration | display json)")
        return 0
    try:
        scan(conf)
    except Shape as e:
        # Nothing is printed then: a part of the objects would look like all.
        fail(f"junos: unexpected configuration layout at {e}")
        return 0
    for line in OUT:
        print(line)
    return 0

if __name__ == "__main__":
    sys.exit(main())
