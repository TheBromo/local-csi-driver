#!/usr/bin/env bash

# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -o errexit
set -o nounset
set -o pipefail

helm_bin="${1:-helm}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart_dir="${repo_root}/charts/latest"
test_tmp_dir="$(mktemp -d)"
readonly helm_bin repo_root chart_dir test_tmp_dir

trap 'rm -rf -- "${test_tmp_dir}"' EXIT

assert_contains() {
    local file="$1"
    local expected="$2"

    if ! grep -Fq -- "${expected}" "${file}"; then
        echo "expected ${file} to contain: ${expected}" >&2
        exit 1
    fi
}

assert_not_contains() {
    local file="$1"
    local unexpected="$2"

    if grep -Fq -- "${unexpected}" "${file}"; then
        echo "expected ${file} not to contain: ${unexpected}" >&2
        exit 1
    fi
}

readonly disabled_output="${test_tmp_dir}/disabled.yaml"
"${helm_bin}" template test "${chart_dir}" --show-only templates/daemonset.yaml >"${disabled_output}"
assert_not_contains "${disabled_output}" "name: node-prep"
assert_not_contains "${disabled_output}" "name: host-etc"
assert_not_contains "${disabled_output}" "--preconfigured-volume-group="

readonly enabled_output="${test_tmp_dir}/enabled.yaml"
"${helm_bin}" template test "${chart_dir}" \
    --show-only templates/daemonset.yaml \
    --set diskPreparation.azureResourceDisk.enabled=true \
    --set diskPreparation.azureResourceDisk.allowDestructivePreparation=true \
    --set diskPreparation.azureResourceDisk.volumeGroup=testvg >"${enabled_output}"
assert_contains "${enabled_output}" "name: node-prep"
assert_contains "${enabled_output}" "command:"
assert_contains "${enabled_output}" "- /local-csi-nodeprep"
assert_contains "${enabled_output}" "--volume-group=testvg"
assert_contains "${enabled_output}" "--preconfigured-volume-group=testvg"
assert_contains "${enabled_output}" "path: /etc"
assert_contains "${enabled_output}" "mountPath: /host/etc"

readonly acknowledgement_error="${test_tmp_dir}/acknowledgement-error.txt"
if "${helm_bin}" template test "${chart_dir}" \
    --set diskPreparation.azureResourceDisk.enabled=true >"${acknowledgement_error}" 2>&1; then
    echo "expected rendering without destructive acknowledgement to fail" >&2
    exit 1
fi
assert_contains "${acknowledgement_error}" \
    "diskPreparation.azureResourceDisk.allowDestructivePreparation must be true"

readonly conflict_error="${test_tmp_dir}/conflict-error.txt"
if "${helm_bin}" template test "${chart_dir}" \
    --set diskPreparation.azureResourceDisk.enabled=true \
    --set diskPreparation.azureResourceDisk.allowDestructivePreparation=true \
    --set raid.enabled=true >"${conflict_error}" 2>&1; then
    echo "expected rendering with RAID and resource-disk preparation to fail" >&2
    exit 1
fi
assert_contains "${conflict_error}" \
    "raid.enabled and diskPreparation.azureResourceDisk.enabled cannot both be true"

readonly raid_output="${test_tmp_dir}/raid.yaml"
"${helm_bin}" template test "${chart_dir}" \
    --show-only templates/daemonset.yaml \
    --set raid.enabled=true >"${raid_output}"
assert_contains "${raid_output}" "name: raid"
assert_not_contains "${raid_output}" "name: node-prep"
