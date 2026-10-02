#!/usr/bin/env python3
"""Load the plugin in a real Traefik the way the plugin catalog does.

Checks that .traefik.yml has the required fields, that its import matches
go.mod, and that Traefik starts the provider with the manifest's testData
(run as a local plugin). Usage: catalog_smoke.py <traefik-binary>
"""
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
REQUIRED = ["displayName", "type", "import", "summary", "testData"]


def fail(msg, log=""):
    print("FAIL: " + msg)
    if log:
        print("---- traefik log ----\n" + log)
    sys.exit(1)


def main():
    traefik = os.path.abspath(sys.argv[1])

    with open(os.path.join(ROOT, ".traefik.yml")) as f:
        manifest = yaml.safe_load(f)
    missing = [k for k in REQUIRED if not manifest.get(k)]
    if missing:
        fail("manifest is missing: " + ", ".join(missing))
    if manifest["type"] != "provider":
        fail("manifest type is %r, expected provider" % manifest["type"])

    with open(os.path.join(ROOT, "go.mod")) as f:
        module = re.search(r"^module\s+(\S+)", f.read(), re.M).group(1)
    if manifest["import"] != module:
        fail("manifest import %r does not match go.mod module %r" % (manifest["import"], module))
    print("manifest OK: %s (%s)" % (manifest["displayName"], module))

    work = tempfile.mkdtemp()
    try:
        dest = os.path.join(work, "plugins-local", "src", module)
        shutil.copytree(ROOT, dest, ignore=shutil.ignore_patterns(".git"))

        static = {
            "entryPoints": {"traefik": {"address": "127.0.0.1:8080"}},
            "api": {"insecure": True},
            "log": {"level": "INFO"},
            "providers": {"plugin": {"cfsync": manifest["testData"]}},
            "experimental": {"localPlugins": {"cfsync": {"moduleName": module}}},
        }
        with open(os.path.join(work, "traefik.yml"), "w") as f:
            yaml.safe_dump(static, f)

        logpath = os.path.join(work, "traefik.log")
        with open(logpath, "w") as logf:
            proc = subprocess.Popen([traefik, "--configFile=traefik.yml"], cwd=work,
                                    stdout=logf, stderr=subprocess.STDOUT)
            deadline = time.time() + 30
            log = ""
            while time.time() < deadline:
                time.sleep(1)
                with open(logpath) as r:
                    log = r.read()
                if "discovered " in log or proc.poll() is not None:
                    break
            if proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=10)
        with open(logpath) as r:
            log = r.read()

        if "Plugins loaded" not in log:
            fail("Traefik did not load the plugin", log)
        if "[plugin-cfsync]" not in log or " init: " not in log:
            fail("the provider was not initialised from testData", log)
        if "discovered " not in log:
            fail("the provider did not read the Traefik API", log)
        if "failed to build provider" in log or "Command error" in log:
            fail("Traefik reported an error building the provider", log)
        if "[plugin-cfsync]" in log and "stopped" not in log:
            fail("the provider did not stop cleanly", log)
        print("Traefik loaded the plugin from testData and it ran:")
        for line in log.splitlines():
            if "[plugin-cfsync]" in line:
                print("  " + line)
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
