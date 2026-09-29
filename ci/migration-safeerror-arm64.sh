#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

# Reviewed, exact candidate inputs. This workflow builds an auditable binary
# candidate only; it does not publish or deploy it.
readonly REVISION="b4a75fbb8faba30d80f3c484ff0bb781c4e7d3e7"
readonly SOURCE_SHA256="fe1f7ae7068c2ef95b92cc24880c302e9de07bb2c6e7a5849264435ca9a549ae"
readonly PATCH_SHA256="c4a7da34c6cda8cb0931b2628ad9f4d4aaef3e819bd2cf298723d876d4176311"
readonly GO_VERSION="1.27.1"
readonly GO_SHA256="3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec"
readonly NODE_VERSION="20.20.2"
readonly NODE_SHA256="73093db209e4e9e09dd7d15a47aeaab1b74833830df03efa5f942a1122c5fa71"
readonly PNPM_VERSION="9.15.9"
readonly PNPM_SHA512="68046141893c66fad01c079231128e9afb89ef87e2691d69e4d40eee228988295fd4682181bae55b58418c3a253bde65a505ec7c5f9403ece5cc3cd37dcf2531"
readonly CANDIDATE_VERSION="0.2.7-custom.2-safeerror.20260928.1"

readonly OUT="${GITHUB_WORKSPACE}/artifacts/migration-safeerror-arm64"
readonly EVIDENCE="${OUT}/evidence"
readonly CANDIDATE="${OUT}/candidate"
readonly WORK="${RUNNER_TEMP}/migration-safeerror-arm64-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
readonly IMAGE_CONTEXT="${GITHUB_WORKSPACE}/image-context-safeerror-arm64"

rm -rf "${OUT}" "${WORK}" "${IMAGE_CONTEXT}"
mkdir -p "${EVIDENCE}" "${CANDIDATE}" "${WORK}" "${IMAGE_CONTEXT}"

on_exit() {
  local rc=$?
  trap - EXIT
  printf '%s\n' "${rc}" > "${EVIDENCE}/workflow.exit"
  exit "${rc}"
}
trap on_exit EXIT

[[ "${NODE_VERSION}" =~ ^20\.[0-9]+\.[0-9]+$ ]] || { printf 'Node must be an exact 20.x.y version\n' >&2; exit 1; }
[[ "${NODE_SHA256}" =~ ^[0-9a-f]{64}$ ]] || { printf 'invalid Node SHA256\n' >&2; exit 1; }
[[ "${PNPM_VERSION}" =~ ^9\.[0-9]+\.[0-9]+$ ]] || { printf 'pnpm must be an exact 9.x.y version\n' >&2; exit 1; }
[[ "${PNPM_SHA512}" =~ ^[0-9a-f]{128}$ ]] || { printf 'invalid pnpm SHA512\n' >&2; exit 1; }
[[ "${CANDIDATE_VERSION}" =~ ^[0-9A-Za-z][0-9A-Za-z.+-]{0,127}$ ]] || { printf 'invalid candidate version\n' >&2; exit 1; }
[[ "${CANDIDATE_VERSION}" != "0.2.7-custom.1" ]] || { printf 'candidate cannot reuse archive VERSION\n' >&2; exit 1; }
[[ "${CANDIDATE_VERSION}" != "0.2.7-custom.2" ]] || { printf 'candidate cannot impersonate historical release version\n' >&2; exit 1; }

check_sha256() {
  local expected=$1 path=$2 label=$3 actual
  actual="$(sha256sum "${path}" | awk '{print $1}')"
  printf '%s  %s\n' "${actual}" "${label}" >> "${EVIDENCE}/verified-sha256.txt"
  if [[ "${actual}" != "${expected}" ]]; then
    printf 'SHA256 mismatch for %s\n' "${label}" >&2
    return 1
  fi
}

check_sha512() {
  local expected=$1 path=$2 label=$3 actual
  actual="$(sha512sum "${path}" | awk '{print $1}')"
  printf '%s  %s\n' "${actual}" "${label}" >> "${EVIDENCE}/verified-sha512.txt"
  if [[ "${actual}" != "${expected}" ]]; then
    printf 'SHA512 mismatch for %s\n' "${label}" >&2
    return 1
  fi
}

safe_extract() {
  local archive=$1 destination=$2 expected_root=$3
  mkdir -p "${destination}"
  python3 - "${archive}" "${destination}" "${expected_root}" <<'PY'
import os
import posixpath
import sys
import tarfile

archive, destination, expected_root = sys.argv[1:]
base = os.path.realpath(destination)
with tarfile.open(archive, "r:*") as tf:
    members = tf.getmembers()
    for member in members:
        raw = member.name.replace("\\", "/")
        name = posixpath.normpath(raw)
        if raw.startswith("/") or name == ".." or name.startswith("../"):
            raise SystemExit("archive path traversal")
        if name != expected_root and not name.startswith(expected_root + "/"):
            raise SystemExit("unexpected archive root")
        target = os.path.realpath(os.path.join(destination, *name.split("/")))
        if os.path.commonpath([base, target]) != base:
            raise SystemExit("archive path traversal")
        if member.issym():
            link = member.linkname.replace("\\", "/")
            if link.startswith("/"):
                raise SystemExit("absolute archive symlink")
            resolved = posixpath.normpath(posixpath.join(posixpath.dirname(name), link))
            if resolved != expected_root and not resolved.startswith(expected_root + "/"):
                raise SystemExit("archive symlink escapes root")
        elif member.islnk():
            link = posixpath.normpath(member.linkname.replace("\\", "/"))
            if link != expected_root and not link.startswith(expected_root + "/"):
                raise SystemExit("archive hardlink escapes root")
        elif not (member.isfile() or member.isdir()):
            raise SystemExit("archive special member rejected")
    tf.extractall(destination, members=members, filter="data")
PY
}

run_logged() {
  local name=$1
  shift
  set +e
  "$@" > "${EVIDENCE}/${name}.log" 2>&1
  local rc=$?
  set -e
  printf '%s\n' "${rc}" > "${EVIDENCE}/${name}.exit"
  if [[ ${rc} -ne 0 ]]; then
    tail -n 200 "${EVIDENCE}/${name}.log" >&2 || true
    return "${rc}"
  fi
}

run_json_gate() {
  local name=$1
  shift
  set +e
  "$@" > "${EVIDENCE}/${name}.jsonl" 2> "${EVIDENCE}/${name}.stderr.log"
  local rc=$?
  set -e
  printf '%s\n' "${rc}" > "${EVIDENCE}/${name}.exit"
  if [[ ${rc} -ne 0 ]]; then
    tail -n 200 "${EVIDENCE}/${name}.jsonl" >&2 || true
    tail -n 200 "${EVIDENCE}/${name}.stderr.log" >&2 || true
    return "${rc}"
  fi
}

check_sha256 "${PATCH_SHA256}" "${GITHUB_WORKSPACE}/ci/SAFE_ERROR_PATCH.v2.diff" "ci/SAFE_ERROR_PATCH.v2.diff"
sha256sum \
  "${GITHUB_WORKSPACE}/.github/workflows/migration-safeerror-arm64.yml" \
  "${GITHUB_WORKSPACE}/ci/migration-safeerror-arm64.sh" \
  "${GITHUB_WORKSPACE}/ci/SAFE_ERROR_PATCH.v2.diff" \
  "${GITHUB_WORKSPACE}/ci/SHA256SUMS.pinned" \
  > "${EVIDENCE}/ci-inputs.sha256"

readonly SOURCE_ARCHIVE="${WORK}/sub2api-${REVISION}.tar.gz"
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 --fail --location --silent --show-error \
  "https://codeload.github.com/Wei-Shaw/sub2api/tar.gz/${REVISION}" \
  --output "${SOURCE_ARCHIVE}" 2> "${EVIDENCE}/source-download.log"
check_sha256 "${SOURCE_SHA256}" "${SOURCE_ARCHIVE}" "sub2api-${REVISION}.tar.gz"
safe_extract "${SOURCE_ARCHIVE}" "${WORK}/source" "sub2api-${REVISION}"
readonly SOURCE_ROOT="${WORK}/source/sub2api-${REVISION}"

# Fixed-layout checks in addition to the whole-archive digest.
check_sha256 "5f602364ad94dbf849ef88fba34ee7365f48c13d6cbe86eac140c4625dab56b5" "${SOURCE_ROOT}/backend/internal/handler/gateway_handler.go" "baseline gateway_handler.go"
check_sha256 "b25c87966a4343132e106a84a8c0bdaf1c2a073a9d5356f19df39d0ed18263b4" "${SOURCE_ROOT}/backend/internal/service/gateway_service.go" "baseline gateway_service.go"
check_sha256 "105595eed2189a9e501d17a5a046fce5b84e0830b3792c7d13479553665cd12d" "${SOURCE_ROOT}/backend/go.mod" "baseline go.mod"
check_sha256 "0ed026ee0ecad45e8c31563a0da61b4f8f00f750b02e32ab159ebe947cadc105" "${SOURCE_ROOT}/backend/go.sum" "baseline go.sum"
check_sha256 "e131e1043017310e58c413e0bb00bd730fc595cc0c3cbc5fbbc16bbdcba8447b" "${SOURCE_ROOT}/frontend/package.json" "frontend package.json"
check_sha256 "8dbd1876020e41b644d971414d29100c9f428f39ede953c03d0442b834f6f3af" "${SOURCE_ROOT}/frontend/pnpm-lock.yaml" "frontend pnpm-lock.yaml"
check_sha256 "815b3dec61328dc35361667718016d2c192dd5236f90fd062c8a5d5293d22538" "${SOURCE_ROOT}/frontend/vite.config.ts" "frontend vite.config.ts"
check_sha256 "b241e67f13ca58445ec683a13176460cc58ac4c8a5a8f18e48b6137b65e63a8a" "${SOURCE_ROOT}/backend/internal/web/embed_on.go" "embed_on.go"
check_sha256 "0d29c161513c785f7ce419aeee8fcfdd50f9dece99a1c53bcafedea37c92820d" "${SOURCE_ROOT}/backend/cmd/server/main.go" "cmd/server/main.go"
check_sha256 "1ff49dbaa8cc30b234dd10fb153c02ac63724cf930923f98f025e95433f79ba8" "${SOURCE_ROOT}/backend/cmd/server/VERSION" "archive VERSION"
[[ "$(tr -d '\r\n' < "${SOURCE_ROOT}/backend/cmd/server/VERSION")" == "0.2.7-custom.1" ]] || { printf 'unexpected archive VERSION content\n' >&2; exit 1; }

if find "${SOURCE_ROOT}/frontend" -maxdepth 1 -type f -name '.env*' -print -quit | grep -q .; then
  printf 'frontend .env input is forbidden\n' >&2
  exit 1
fi
rm -rf "${SOURCE_ROOT}/backend/internal/web/dist"

git -C "${SOURCE_ROOT}" apply --check "${GITHUB_WORKSPACE}/ci/SAFE_ERROR_PATCH.v2.diff" > "${EVIDENCE}/patch-check.log" 2>&1
git -C "${SOURCE_ROOT}" apply "${GITHUB_WORKSPACE}/ci/SAFE_ERROR_PATCH.v2.diff" > "${EVIDENCE}/patch-apply.log" 2>&1
check_sha256 "55f58467c94e57bb00d50dfc5c788bad6a4713984377fbc8a639009e96c5df99" "${SOURCE_ROOT}/backend/internal/handler/gateway_handler.go" "patched gateway_handler.go"
check_sha256 "113b52a44159e681cab4686ca801b48352966dbb7dd866f137d2b69e8fc358a9" "${SOURCE_ROOT}/backend/internal/handler/gateway_safe_error_projection_test.go" "patched gateway_safe_error_projection_test.go"
check_sha256 "338fa5266c23e8c00eca4452127b03e4f3147bbcf954d15fedae97f30bf2fb2f" "${SOURCE_ROOT}/backend/internal/pkg/safeerrorprojection/safe_error_projection.go" "patched safe_error_projection.go"
check_sha256 "218a865d31f526fcf999a309a4f74fc5cf4069a89a86c3d2ad526753d7cbf65b" "${SOURCE_ROOT}/backend/internal/pkg/safeerrorprojection/safe_error_projection_test.go" "patched safe_error_projection_test.go"

readonly GO_ARCHIVE="${WORK}/go${GO_VERSION}.linux-arm64.tar.gz"
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 --fail --location --silent --show-error \
  "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz" \
  --output "${GO_ARCHIVE}" 2> "${EVIDENCE}/go-download.log"
check_sha256 "${GO_SHA256}" "${GO_ARCHIVE}" "go${GO_VERSION}.linux-arm64.tar.gz"
safe_extract "${GO_ARCHIVE}" "${WORK}/go-toolchain" "go"

export GOROOT="${WORK}/go-toolchain/go"
export PATH="${GOROOT}/bin:/usr/local/bin:/usr/bin:/bin"
export HOME="${WORK}/go-home"
export GOMODCACHE="${WORK}/go-mod-cache"
export GOCACHE="${WORK}/go-build-cache"
export GOTOOLCHAIN=local
export GOWORK=off
export GOPROXY=https://proxy.golang.org
export GOSUMDB=sum.golang.org
export GOFLAGS='-mod=readonly -p=2'
export CGO_ENABLED=0
export GOOS=linux
export GOARCH=arm64
mkdir -p "${HOME}" "${GOMODCACHE}" "${GOCACHE}"

cd "${SOURCE_ROOT}/backend"
go version > "${EVIDENCE}/go-version.log"
[[ "$(go version)" == "go version go${GO_VERSION} linux/arm64" ]] || { printf 'unexpected Go toolchain\n' >&2; exit 1; }
go env GOOS GOARCH GOROOT GOTOOLCHAIN GOPROXY GOSUMDB GOFLAGS CGO_ENABLED GOMODCACHE GOCACHE > "${EVIDENCE}/go-build-settings.log"

if grep -R --include='*_test.go' -n 'func TestMain' internal/handler internal/service internal/pkg/safeerrorprojection > "${EVIDENCE}/selected-packages-testmain.log"; then
  printf 'selected packages unexpectedly define TestMain\n' >&2
  exit 1
else
  printf 'No TestMain in selected packages.\n' > "${EVIDENCE}/selected-packages-testmain.log"
fi

run_logged go-mod-download timeout --kill-after=15s 600s go mod download
run_logged go-mod-verify timeout --kill-after=15s 300s go mod verify
run_json_gate candidate-compile timeout --kill-after=15s 600s go test -json -count=1 -run '^$' ./internal/handler ./internal/service
run_json_gate helper-tests timeout --kill-after=15s 240s go test -json -count=1 ./internal/pkg/safeerrorprojection
run_json_gate handler-safe-error-tests timeout --kill-after=15s 240s go test -json -count=1 -run '^TestSafeErrorProjection' ./internal/handler
run_json_gate handler-stream-regression-tests timeout --kill-after=15s 240s go test -json -count=1 -run '^(TestStreamWrittenGuard_MessagesPath_AbortFailoverOnSSEContentWritten|TestStreamWrittenGuard_GeminiPath_AbortFailoverOnSSEContentWritten|TestStreamWrittenGuard_NoByteWritten_GuardNotTriggered)$' ./internal/handler

python3 - "${EVIDENCE}" <<'PY'
import json
import os
import sys

evidence = sys.argv[1]
module = "github.com/Wei-Shaw/sub2api"
specs = {
    "helper-tests": {
        "packages": {module + "/internal/pkg/safeerrorprojection"},
        "tests": {
            "TestMatchTrustedOracle429ScopeAndSelector",
            "TestTrustedSourceRequiresExactServerSideEndpoint",
            "TestMatchTrustedOracle429RejectsUnsafeOrUnavailableMessages",
            "TestMatchTrustedOracle429IgnoresUnknownFields",
            "TestMatchTrustedOracle429RejectsOversizedEnvelope",
        },
    },
    "handler-safe-error-tests": {
        "packages": {module + "/internal/handler"},
        "tests": {
            "TestSafeErrorProjectionDirectJSON",
            "TestSafeErrorProjectionCommittedSSE",
            "TestSafeErrorProjectionScopeMismatchUsesLegacyGeneric",
            "TestSafeErrorProjectionMessageUnavailableUsesFixedText",
            "TestSafeErrorProjectionScopeDoesNotLeakAcrossCalls",
        },
    },
    "handler-stream-regression-tests": {
        "packages": {module + "/internal/handler"},
        "tests": {
            "TestStreamWrittenGuard_MessagesPath_AbortFailoverOnSSEContentWritten",
            "TestStreamWrittenGuard_GeminiPath_AbortFailoverOnSSEContentWritten",
            "TestStreamWrittenGuard_NoByteWritten_GuardNotTriggered",
        },
    },
}

def events(name):
    result = []
    with open(os.path.join(evidence, name + ".jsonl"), encoding="utf-8") as f:
        for number, line in enumerate(f, 1):
            try:
                result.append(json.loads(line))
            except json.JSONDecodeError as exc:
                raise SystemExit(f"{name}: non-JSON stdout line {number}: {exc}")
    return result

compile_events = events("candidate-compile")
compile_tests = {e["Test"] for e in compile_events if e.get("Test")}
compile_packages = {e["Package"] for e in compile_events if e.get("Action") == "pass" and not e.get("Test")}
expected_compile = {module + "/internal/handler", module + "/internal/service"}
if compile_tests or compile_packages != expected_compile:
    raise SystemExit("compile-only gate did not pass exactly two packages with zero test events")

summary = {
    "candidate_compile": {"package_pass_count": 2, "test_event_count": 0},
    "test_gates": {},
}
for name, spec in specs.items():
    evs = events(name)
    skipped = sorted({e["Test"] for e in evs if e.get("Action") == "skip" and e.get("Test")})
    failed = sorted({e["Test"] for e in evs if e.get("Action") == "fail" and e.get("Test")})
    passed = {e["Test"] for e in evs if e.get("Action") == "pass" and e.get("Test")}
    top_level = {test for test in passed if "/" not in test}
    package_passes = {e["Package"] for e in evs if e.get("Action") == "pass" and not e.get("Test")}
    if skipped or failed or top_level != spec["tests"] or package_passes != spec["packages"]:
        raise SystemExit(f"{name}: runtime test selection/pass audit failed")
    summary["test_gates"][name] = {
        "package_pass_count": len(package_passes),
        "top_level_pass_count": len(top_level),
        "subtest_pass_count": len(passed - top_level),
        "skip_count": 0,
        "top_level_passed": sorted(top_level),
    }
with open(os.path.join(evidence, "test-execution-summary.json"), "w", encoding="utf-8", newline="\n") as f:
    json.dump(summary, f, indent=2, sort_keys=True)
    f.write("\n")
PY

# Fetch exact, reviewed frontend toolchains without setup actions or Corepack drift.
readonly NODE_ARCHIVE="${WORK}/node-v${NODE_VERSION}-linux-arm64.tar.xz"
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 --fail --location --silent --show-error \
  "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-arm64.tar.xz" \
  --output "${NODE_ARCHIVE}" 2> "${EVIDENCE}/node-download.log"
check_sha256 "${NODE_SHA256}" "${NODE_ARCHIVE}" "node-v${NODE_VERSION}-linux-arm64.tar.xz"
safe_extract "${NODE_ARCHIVE}" "${WORK}/node-toolchain" "node-v${NODE_VERSION}-linux-arm64"
readonly NODE_ROOT="${WORK}/node-toolchain/node-v${NODE_VERSION}-linux-arm64"
readonly NODE_BIN="${NODE_ROOT}/bin/node"
[[ "$(${NODE_BIN} --version)" == "v${NODE_VERSION}" ]] || { printf 'unexpected Node toolchain\n' >&2; exit 1; }

readonly PNPM_ARCHIVE="${WORK}/pnpm-${PNPM_VERSION}.tgz"
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 --fail --location --silent --show-error \
  "https://registry.npmjs.org/pnpm/-/pnpm-${PNPM_VERSION}.tgz" \
  --output "${PNPM_ARCHIVE}" 2> "${EVIDENCE}/pnpm-download.log"
check_sha512 "${PNPM_SHA512}" "${PNPM_ARCHIVE}" "pnpm-${PNPM_VERSION}.tgz"
safe_extract "${PNPM_ARCHIVE}" "${WORK}/pnpm-toolchain" "package"
readonly PNPM_ENTRY="${WORK}/pnpm-toolchain/package/bin/pnpm.cjs"
[[ -f "${PNPM_ENTRY}" ]] || { printf 'pnpm entrypoint missing\n' >&2; exit 1; }

mkdir -p "${WORK}/frontend-home" "${WORK}/frontend-tmp" "${WORK}/pnpm-store" "${WORK}/xdg-config" "${WORK}/xdg-cache" "${WORK}/frontend-tool-bin"
readonly PNPM_WRAPPER="${WORK}/frontend-tool-bin/pnpm"
cat > "${PNPM_WRAPPER}" <<SH
#!/usr/bin/env bash
set -euo pipefail
exec env -i \
  CI=true \
  HOME="${WORK}/frontend-home" \
  TMPDIR="${WORK}/frontend-tmp" \
  XDG_CONFIG_HOME="${WORK}/xdg-config" \
  XDG_CACHE_HOME="${WORK}/xdg-cache" \
  PATH="${WORK}/frontend-tool-bin:${NODE_ROOT}/bin:/usr/bin:/bin" \
  npm_config_registry="https://registry.npmjs.org" \
  npm_config_userconfig="/dev/null" \
  npm_config_audit="false" \
  npm_config_fund="false" \
  COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
  "${NODE_BIN}" "${PNPM_ENTRY}" "\$@"
SH
chmod 0755 "${PNPM_WRAPPER}"
grep -Fq "${NODE_BIN}" "${PNPM_WRAPPER}"
grep -Fq "${PNPM_ENTRY}" "${PNPM_WRAPPER}"
[[ "$(env -i PATH="${WORK}/frontend-tool-bin:${NODE_ROOT}/bin:/usr/bin:/bin" /bin/sh -c 'command -v pnpm')" == "${PNPM_WRAPPER}" ]] || {
  printf 'nested pnpm does not resolve to the pinned wrapper\n' >&2
  exit 1
}
if grep -Eqi '(^|[[:space:]/])corepack([[:space:]]|$)|/usr/(local/)?bin/pnpm' "${PNPM_WRAPPER}"; then
  printf 'pnpm wrapper contains a forbidden fallback\n' >&2
  exit 1
fi

{
  "${NODE_BIN}" --version
  "${PNPM_WRAPPER}" --version
} > "${EVIDENCE}/frontend-toolchain-versions.log"
[[ "$(tail -n 1 "${EVIDENCE}/frontend-toolchain-versions.log")" == "${PNPM_VERSION}" ]] || { printf 'unexpected pnpm toolchain\n' >&2; exit 1; }

run_logged frontend-install timeout --kill-after=30s 900s "${PNPM_WRAPPER}" --dir "${SOURCE_ROOT}/frontend" install --frozen-lockfile --store-dir "${WORK}/pnpm-store"
run_logged frontend-build timeout --kill-after=30s 900s "${PNPM_WRAPPER}" --dir "${SOURCE_ROOT}/frontend" run build
check_sha256 "e131e1043017310e58c413e0bb00bd730fc595cc0c3cbc5fbbc16bbdcba8447b" "${SOURCE_ROOT}/frontend/package.json" "post-build frontend package.json"
check_sha256 "8dbd1876020e41b644d971414d29100c9f428f39ede953c03d0442b834f6f3af" "${SOURCE_ROOT}/frontend/pnpm-lock.yaml" "post-build frontend pnpm-lock.yaml"

readonly DIST="${SOURCE_ROOT}/backend/internal/web/dist"
python3 - "${DIST}" "${EVIDENCE}/frontend-assets.sha256" "${EVIDENCE}/frontend-assets.tsv" "${EVIDENCE}/frontend-sensitive-marker-scan.log" <<'PY'
import hashlib
import os
import re
import sys

dist, sums_path, tsv_path, scan_path = sys.argv[1:]
if not os.path.isdir(dist):
    raise SystemExit("frontend dist missing")
entries = []
for current, dirs, files in os.walk(dist, followlinks=False):
    for name in dirs + files:
        path = os.path.join(current, name)
        if os.path.islink(path):
            raise SystemExit("frontend dist symlink rejected")
    for name in files:
        path = os.path.join(current, name)
        rel = os.path.relpath(path, dist).replace(os.sep, "/")
        if any(ord(ch) < 32 or ord(ch) == 127 for ch in rel):
            raise SystemExit("control character in frontend asset path")
        if rel.endswith(".map"):
            raise SystemExit("source map asset rejected")
        data = open(path, "rb").read()
        if b"sourceMappingURL=" in data:
            raise SystemExit("sourceMappingURL reference rejected")
        entries.append((rel, len(data), hashlib.sha256(data).hexdigest(), data))
if not entries or not any(rel == "index.html" for rel, *_ in entries):
    raise SystemExit("real frontend build must contain index.html")
index = next(data for rel, _, _, data in entries if rel == "index.html")
if b"<script" not in index or b"assets/" not in index:
    raise SystemExit("index.html does not reference built assets")
patterns = [
    rb"-----BEGIN (?:RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----",
    rb"github_pat_[A-Za-z0-9_]{20,}",
    rb"ghp_[A-Za-z0-9]{36}",
    rb"AKIA[0-9A-Z]{16}",
]
for rel, _, _, data in entries:
    for pattern in patterns:
        if re.search(pattern, data):
            raise SystemExit("high-confidence sensitive marker in frontend asset: " + rel)
entries.sort()
with open(sums_path, "w", encoding="ascii", newline="\n") as sums, open(tsv_path, "w", encoding="utf-8", newline="\n") as tsv:
    tsv.write("sha256\tsize_bytes\tpath\n")
    for rel, size, digest, _ in entries:
        sums.write(f"{digest}  {rel}\n")
        tsv.write(f"{digest}\t{size}\t{rel}\n")
with open(scan_path, "w", encoding="ascii", newline="\n") as scan:
    scan.write("No source maps or configured high-confidence sensitive markers found.\n")
    scan.write("This is a bounded marker scan, not a proof that arbitrary content is non-sensitive.\n")
PY

# Compile and run one source-provided embed assertion against the actual dist.
run_json_gate frontend-embed-test timeout --kill-after=15s 300s go test -json -count=1 -tags=embed -run '^TestHasEmbeddedFrontend$' ./internal/web
python3 - "${EVIDENCE}/frontend-embed-test.jsonl" <<'PY'
import json
import sys

events = [json.loads(line) for line in open(sys.argv[1], encoding="utf-8")]
passed = {e.get("Test") for e in events if e.get("Action") == "pass" and e.get("Test")}
skipped = {e.get("Test") for e in events if e.get("Action") == "skip" and e.get("Test")}
if "TestHasEmbeddedFrontend" not in passed or skipped:
    raise SystemExit("embedded frontend assertion did not execute and pass without skips")
PY

# Match historical release mechanics by replacing VERSION only in the ephemeral
# fixed-source worktree, while giving this non-published candidate a distinct version.
printf '%s\n' "${CANDIDATE_VERSION}" > "${SOURCE_ROOT}/backend/cmd/server/VERSION"
readonly CANDIDATE_VERSION_SHA256="$(sha256sum "${SOURCE_ROOT}/backend/cmd/server/VERSION" | awk '{print $1}')"
printf '%s  candidate backend/cmd/server/VERSION\n' "${CANDIDATE_VERSION_SHA256}" >> "${EVIDENCE}/verified-sha256.txt"

check_sha256 "105595eed2189a9e501d17a5a046fce5b84e0830b3792c7d13479553665cd12d" "${SOURCE_ROOT}/backend/go.mod" "final go.mod"
check_sha256 "0ed026ee0ecad45e8c31563a0da61b4f8f00f750b02e32ab159ebe947cadc105" "${SOURCE_ROOT}/backend/go.sum" "final go.sum"

readonly BUILD_EPOCH="$(date -u +%s)"
readonly BUILD_DATE="$(date -u -d "@${BUILD_EPOCH}" +'%Y-%m-%dT%H:%M:%SZ')"
readonly PACKAGE_STAGE="${WORK}/package-stage"
readonly BINARY="${PACKAGE_STAGE}/sub2api"
mkdir -p "${PACKAGE_STAGE}"
readonly LDFLAGS="-s -w -X main.Version=${CANDIDATE_VERSION} -X main.Commit=${REVISION} -X main.Date=${BUILD_DATE} -X main.BuildType=release"
printf '%q ' env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags=embed -trimpath -buildvcs=false "-ldflags=${LDFLAGS}" -o sub2api ./cmd/server > "${EVIDENCE}/binary-build-command.log"
printf '\n' >> "${EVIDENCE}/binary-build-command.log"
run_logged candidate-binary-build timeout --kill-after=30s 900s go build -tags=embed -trimpath -buildvcs=false "-ldflags=${LDFLAGS}" -o "${BINARY}" ./cmd/server
chmod 0755 "${BINARY}"

file "${BINARY}" > "${EVIDENCE}/binary-file.log"
readelf -h "${BINARY}" > "${EVIDENCE}/binary-elf-header.log"
go version -m "${BINARY}" > "${EVIDENCE}/binary-go-version-m.log"
go tool buildid "${BINARY}" > "${EVIDENCE}/binary-go-buildid.log"
timeout --kill-after=2s 10s "${BINARY}" -version > "${EVIDENCE}/binary-version.log" 2>&1

grep -Fq 'ELF 64-bit' "${EVIDENCE}/binary-file.log"
grep -Eq 'ARM aarch64|aarch64' "${EVIDENCE}/binary-file.log"
grep -Fq 'Class:                             ELF64' "${EVIDENCE}/binary-elf-header.log"
grep -Fq 'Machine:                           AArch64' "${EVIDENCE}/binary-elf-header.log"
grep -Fq 'CGO_ENABLED=0' "${EVIDENCE}/binary-go-version-m.log"
grep -Fq 'GOOS=linux' "${EVIDENCE}/binary-go-version-m.log"
grep -Fq 'GOARCH=arm64' "${EVIDENCE}/binary-go-version-m.log"
grep -Fq -- '-tags=embed' "${EVIDENCE}/binary-go-version-m.log"
grep -Fq "Sub2API ${CANDIDATE_VERSION} (commit: ${REVISION}, built: ${BUILD_DATE})" "${EVIDENCE}/binary-version.log"

{
  printf 'artifact_class=migration-only-arm64-image-input\n'
  printf 'candidate_version=%s\n' "${CANDIDATE_VERSION}"
  printf 'upstream_revision=%s\n' "${REVISION}"
  printf 'patch_sha256=%s\n' "${PATCH_SHA256}"
  printf 'build_date=%s\n' "${BUILD_DATE}"
  printf 'build_type=release\n'
  printf 'cgo_enabled=0\n'
  printf 'goos=linux\n'
  printf 'goarch=arm64\n'
  printf 'go_version=%s\n' "${GO_VERSION}"
  printf 'node_version=%s\n' "${NODE_VERSION}"
  printf 'pnpm_version=%s\n' "${PNPM_VERSION}"
  printf 'github_run_id=%s\n' "${GITHUB_RUN_ID}"
  printf 'github_run_attempt=%s\n' "${GITHUB_RUN_ATTEMPT}"
  printf 'ci_head_sha=%s\n' "${GITHUB_SHA}"
  printf '\n[go version -m]\n'
  cat "${EVIDENCE}/binary-go-version-m.log"
  printf '\n[go build id]\n'
  cat "${EVIDENCE}/binary-go-buildid.log"
  printf '\n[binary -version]\n'
  cat "${EVIDENCE}/binary-version.log"
} > "${EVIDENCE}/build-info.txt"

cp "${EVIDENCE}/build-info.txt" "${PACKAGE_STAGE}/BUILD-INFO.txt"
cp "${EVIDENCE}/frontend-assets.sha256" "${PACKAGE_STAGE}/FRONTEND-ASSETS.sha256"
cp "${EVIDENCE}/frontend-assets.tsv" "${PACKAGE_STAGE}/FRONTEND-ASSETS.tsv"
cp "${EVIDENCE}/test-execution-summary.json" "${PACKAGE_STAGE}/TEST-EXECUTION-SUMMARY.json"

export MANIFEST_BINARY="${BINARY}"
export MANIFEST_OUTPUT="${PACKAGE_STAGE}/MANIFEST.json"
export MANIFEST_FRONTEND="${EVIDENCE}/frontend-assets.sha256"
export MANIFEST_TESTS="${EVIDENCE}/test-execution-summary.json"
export MANIFEST_CI_INPUTS="${EVIDENCE}/ci-inputs.sha256"
export MANIFEST_VERSION_SHA="${CANDIDATE_VERSION_SHA256}"
export MANIFEST_BUILD_DATE="${BUILD_DATE}"
export MANIFEST_BUILD_EPOCH="${BUILD_EPOCH}"
export MANIFEST_NODE_VERSION="${NODE_VERSION}"
export MANIFEST_NODE_SHA="${NODE_SHA256}"
export MANIFEST_PNPM_VERSION="${PNPM_VERSION}"
export MANIFEST_PNPM_SHA512="${PNPM_SHA512}"
export MANIFEST_CANDIDATE_VERSION="${CANDIDATE_VERSION}"
export MANIFEST_REVISION="${REVISION}"
export MANIFEST_SOURCE_SHA="${SOURCE_SHA256}"
export MANIFEST_PATCH_SHA="${PATCH_SHA256}"
export MANIFEST_GO_VERSION="${GO_VERSION}"
export MANIFEST_GO_SHA="${GO_SHA256}"
python3 <<'PY'
import hashlib
import json
import os


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()

binary = os.environ["MANIFEST_BINARY"]
manifest = {
    "schema": "sub2api-binary-candidate/v1",
    "classification": "migration-only-arm64-image-input",
    "approval": "G15/r58 approved migration-only ARM64 safeerror artifact; no production tag overwrite",
    "source": {
        "repository": "https://github.com/Wei-Shaw/sub2api",
        "revision": os.environ["MANIFEST_REVISION"],
        "archive_sha256": os.environ["MANIFEST_SOURCE_SHA"],
        "archive_version": "0.2.7-custom.1",
        "historical_release_context_from_main_review": {
            "run_id": 35814584179,
            "version": "0.2.7-custom.2",
            "head_revision": os.environ["MANIFEST_REVISION"],
        },
    },
    "patch": {
        "name": "SAFE_ERROR_PATCH.v2.diff",
        "sha256": os.environ["MANIFEST_PATCH_SHA"],
    },
    "version": {
        "candidate": os.environ["MANIFEST_CANDIDATE_VERSION"],
        "ephemeral_version_file_sha256": os.environ["MANIFEST_VERSION_SHA"],
        "commit_ldflag": os.environ["MANIFEST_REVISION"],
        "date_ldflag": os.environ["MANIFEST_BUILD_DATE"],
        "build_type_ldflag": "release",
    },
    "toolchains": {
        "go": {"version": os.environ["MANIFEST_GO_VERSION"], "archive_sha256": os.environ["MANIFEST_GO_SHA"], "historical_safeerror_toolchain": "1.27.1"},
        "node": {"version": os.environ["MANIFEST_NODE_VERSION"], "archive_sha256": os.environ["MANIFEST_NODE_SHA"]},
        "pnpm": {"version": os.environ["MANIFEST_PNPM_VERSION"], "npm_tarball_sha512": os.environ["MANIFEST_PNPM_SHA512"]},
    },
    "target": {"goos": "linux", "goarch": "arm64", "cgo_enabled": False, "build_tags": ["embed"]},
    "build": {
        "utc": os.environ["MANIFEST_BUILD_DATE"],
        "epoch": int(os.environ["MANIFEST_BUILD_EPOCH"]),
        "trimpath": True,
        "buildvcs": False,
        "go_modules": "readonly",
        "frontend_environment": "allowlisted-clean-env",
    },
    "binary": {"name": "sub2api", "size_bytes": os.path.getsize(binary), "sha256": digest(binary)},
    "evidence": {
        "frontend_assets_sha256": digest(os.environ["MANIFEST_FRONTEND"]),
        "test_execution_summary_sha256": digest(os.environ["MANIFEST_TESTS"]),
        "ci_inputs_sha256_file_sha256": digest(os.environ["MANIFEST_CI_INPUTS"]),
    },
    "ci": {
        "repository": os.environ.get("GITHUB_REPOSITORY", ""),
        "head_sha": os.environ.get("GITHUB_SHA", ""),
        "run_id": os.environ.get("GITHUB_RUN_ID", ""),
        "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT", ""),
        "runner": "github-hosted ubuntu-24.04-arm (native aarch64)",
        "permissions": "contents:read,packages:write",
    },
}
with open(os.environ["MANIFEST_OUTPUT"], "w", encoding="utf-8", newline="\n") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write("\n")
PY
cp "${PACKAGE_STAGE}/MANIFEST.json" "${EVIDENCE}/MANIFEST.json"

(
  cd "${PACKAGE_STAGE}"
  sha256sum BUILD-INFO.txt FRONTEND-ASSETS.sha256 FRONTEND-ASSETS.tsv MANIFEST.json TEST-EXECUTION-SUMMARY.json sub2api > SHA256SUMS
)
readonly ARCHIVE_NAME="sub2api-${CANDIDATE_VERSION}-linux-arm64.tar.gz"
tar --sort=name --mtime="@${BUILD_EPOCH}" --owner=0 --group=0 --numeric-owner -C "${PACKAGE_STAGE}" -cf - . \
  | gzip -n > "${CANDIDATE}/${ARCHIVE_NAME}"
sha256sum "${CANDIDATE}/${ARCHIVE_NAME}" | sed 's#  .*/#  #' > "${CANDIDATE}/${ARCHIVE_NAME}.sha256"

{
  printf 'Migration-only ARM64 package and image context built; no source release or deployment performed.\n'
  printf 'archive=%s\n' "${ARCHIVE_NAME}"
  printf 'archive_size_bytes=%s\n' "$(stat -c '%s' "${CANDIDATE}/${ARCHIVE_NAME}")"
  cat "${CANDIDATE}/${ARCHIVE_NAME}.sha256"
} > "${EVIDENCE}/result.txt"

# Package the exact tested binary with the source Dockerfile/runtime resources.
cp "${SOURCE_ROOT}/Dockerfile.goreleaser" "${IMAGE_CONTEXT}/Dockerfile"
mkdir -p "${IMAGE_CONTEXT}/backend" "${IMAGE_CONTEXT}/deploy"
cp -a "${SOURCE_ROOT}/backend/resources" "${IMAGE_CONTEXT}/backend/resources"
cp "${SOURCE_ROOT}/deploy/docker-entrypoint.sh" "${IMAGE_CONTEXT}/deploy/docker-entrypoint.sh"
cp "${BINARY}" "${IMAGE_CONTEXT}/sub2api"
chmod 0755 "${IMAGE_CONTEXT}/sub2api" "${IMAGE_CONTEXT}/deploy/docker-entrypoint.sh"
sha256sum "${IMAGE_CONTEXT}/sub2api" > "${EVIDENCE}/image-context-binary.sha256"
[[ "$(sha256sum "${BINARY}" | awk '{print $1}')" == "$(sha256sum "${IMAGE_CONTEXT}/sub2api" | awk '{print $1}')" ]]

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  printf 'binary_sha256=%s\n' "$(sha256sum "${BINARY}" | awk '{print $1}')" >> "${GITHUB_OUTPUT}"
  printf 'manifest_sha256=%s\n' "$(sha256sum "${PACKAGE_STAGE}/MANIFEST.json" | awk '{print $1}')" >> "${GITHUB_OUTPUT}"
  printf 'build_date=%s\n' "${BUILD_DATE}" >> "${GITHUB_OUTPUT}"
fi
printf '0\n' > "${EVIDENCE}/workflow.exit"
