#!/bin/sh
# Builds the argus-agent macOS installer from an argus-agent binary.
#
#   ./build-pkg.sh <argus-agent> <version> <out.pkg>
#
# Runs on macOS (pkgbuild is Apple's). The release builds from the binary in
# its own darwin archive, so the installer and the archive put the same file
# in place. With DEVELOPER_ID_INSTALLER set to a "Developer ID Installer: …"
# identity in the keychain the package is signed with it; without one it is
# unsigned, and Gatekeeper asks before it opens (right-click › Open).
set -eu
[ $# -eq 3 ] || { echo "usage: $0 <argus-agent> <version> <out.pkg>" >&2; exit 2; }
exe=$1 version=${2#v} out=$3
here=$(cd "$(dirname "$0")" && pwd)

case $version in
[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "not a version: $version" >&2; exit 2 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root=$work/root
install -d -m 0755 "$root/usr/local/bin" "$root/Library/LaunchDaemons"
install -m 0755 "$exe" "$root/usr/local/bin/argus-agent"
install -m 0644 "$here/../launchd/com.argus.agent.plist" "$root/Library/LaunchDaemons/"
plutil -lint "$root/Library/LaunchDaemons/com.argus.agent.plist" >/dev/null

scripts=$work/scripts
install -d "$scripts"
install -m 0755 "$here/scripts/preinstall" "$here/scripts/postinstall" "$scripts/"

unsigned=$work/argus-agent.pkg
pkgbuild --root "$root" --scripts "$scripts" --identifier com.argus.agent \
	--version "$version" --install-location / --ownership recommended "$unsigned"

if [ -n "${DEVELOPER_ID_INSTALLER:-}" ]; then
	productsign --sign "$DEVELOPER_ID_INSTALLER" "$unsigned" "$out"
else
	cp "$unsigned" "$out"
fi
echo "built $out ($version)"
