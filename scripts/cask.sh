#!/bin/sh
# Renders the Homebrew cask of a release from its checksums.txt:
#   scripts/cask.sh VERSION CHECKSUMS > tforphans.rb
# VERSION is without the leading v (0.1.10).
set -eu

if [ $# -ne 2 ]; then
	echo "usage: $0 VERSION CHECKSUMS" >&2
	exit 2
fi
version=$1
sums=$2
case $version in
*[!0-9.]* | "" | .* | *. | *..*)
	echo "$0: bad version: $version" >&2
	exit 2
	;;
esac

sha() {
	file="tforphans_${version}_$1.tar.gz"
	s=$(awk -v f="$file" '$2 == f { print $1 }' "$sums")
	if [ -z "$s" ]; then
		echo "$0: no checksum for $file" >&2
		exit 1
	fi
	# The value goes into Ruby: only 64 hex digits, one line.
	case $s in
	*[!0-9a-f]*)
		echo "$0: bad checksum for $file" >&2
		exit 1
		;;
	esac
	if [ ${#s} -ne 64 ]; then
		echo "$0: bad checksum for $file" >&2
		exit 1
	fi
	echo "$s"
}

darwin_arm64=$(sha darwin_arm64)
darwin_amd64=$(sha darwin_amd64)
linux_arm64=$(sha linux_arm64)
linux_amd64=$(sha linux_amd64)

cat <<CASK
# Written by scripts/cask.sh in 5xxeu/tforphans on each release. DO NOT EDIT.
cask "tforphans" do
  version "$version"

  on_macos do
    on_arm do
      sha256 "$darwin_arm64"
      url "https://github.com/5xxeu/tforphans/releases/download/v#{version}/tforphans_#{version}_darwin_arm64.tar.gz"
    end
    on_intel do
      sha256 "$darwin_amd64"
      url "https://github.com/5xxeu/tforphans/releases/download/v#{version}/tforphans_#{version}_darwin_amd64.tar.gz"
    end
  end
  on_linux do
    on_arm do
      sha256 "$linux_arm64"
      url "https://github.com/5xxeu/tforphans/releases/download/v#{version}/tforphans_#{version}_linux_arm64.tar.gz"
    end
    on_intel do
      sha256 "$linux_amd64"
      url "https://github.com/5xxeu/tforphans/releases/download/v#{version}/tforphans_#{version}_linux_amd64.tar.gz"
    end
  end

  name "tforphans"
  desc "Find cloud resources that no Terraform state manages"
  homepage "https://github.com/5xxeu/tforphans"

  livecheck do
    skip "Written on release."
  end

  binary "tforphans"

  # The binaries are not notarized: drop the quarantine flag that brew sets
  # on downloads, or macOS refuses to run them.
  postflight_steps do
    on_macos do
      run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{staged_path}}/tforphans"]
    end
  end
end
CASK
