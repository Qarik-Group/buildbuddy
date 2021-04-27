#!/bin/bash
set -e

# Vendors go repository build files and outputs a replacement http_archive.
#
# usage ./tools/vendor.sh <org/repo> <commit> [custom-import-path] [custom-name]
# example: ./tools/vendor.sh docker/docker 363e9a88a11be517d9e8c65c998ff56f774eb4dc
# example: ./tools/vendor_git.sh googleapis/google-api-go-client c0067489eddd6a0c8aad7c6f9ac0ebd946c0f3d8 google.golang.org/api org_golang_google_api

if [[ $# -eq 0 ]]; then
    echo """
    usage ./tools/vendor.sh <org/repo> <commit> [custom-import-path] [custom-name]
    example: ./tools/vendor.sh docker/docker 363e9a88a11be517d9e8c65c998ff56f774eb4dc
    example: ./tools/vendor_git.sh googleapis/google-api-go-client c0067489eddd6a0c8aad7c6f9ac0ebd946c0f3d8 google.golang.org/api org_golang_google_api
    """
    exit
fi

pwd=$(pwd)

# Make sure directory exists
mkdir -p buildpatches

# Create temp directory
tmpdir=$(mktemp -d)
mkdir -p $tmpdir
cd $tmpdir
cleanup() { 
    rm -rf "$tmpdir" 
}
trap cleanup EXIT

# Figure out repo name and url
reponame=$(basename "$1" .git)
archive_url="https://github.com/$1/archive/$2.zip"

# Download the archive
curl -fsSL -o $reponame.zip $archive_url

# Calculate the sha256
sha256=$(openssl dgst -sha256 $reponame.zip | awk '{print $2}')

# Unzip the archive
unzip $reponame.zip

# Go into the unzipped directory
root_dir=$(unzip -Z -1 $reponame.zip | head -1)
cd $root_dir

# Fallback to github repo as prefix
github_prefix="github.com/$1"
custom_prefix=${3-$github_prefix}

# Fallback to github repo as name
github_name="com_github_${1//[^[:alnum:]]/_}"
custom_name=${4-$github_name}

# Run gazelle to generate the build file patch
$(go env GOPATH)/bin/gazelle -go_repository_mode -go_prefix $custom_prefix -mode diff -repo_root . -go_repository_module_mode -go_naming_convention import_alias > $pwd/buildpatches/$custom_name || true # diff prints error code base on diffs, not failure

# Print out the git_repository for deps.bzl
echo """
Add this to your deps.bzl:

    # Generated with ./tools/vendor.sh $@
    http_archive(
        name = \"$custom_name\",
        sha256 = \"$sha256\",
        strip_prefix = \"${root_dir%/}\",
        urls = [\"$archive_url\"],
        patches = [\"@%s//buildpatches:$custom_name\" % workspace_name],
        patch_args = [\"-s\", \"-p0\"],
    )
"""
