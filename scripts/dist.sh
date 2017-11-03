#!/bin/bash
set -e

# Get the version from the environment, or try to figure it out.
if [ -z $VERSION ]; then
	VERSION=$(awk -F\" '/Version =/ { print $2; exit }' < version/version.go)
fi
if [ -z "$VERSION" ]; then
    echo "Please specify a version."
    exit 1
fi
echo "==> Building version $VERSION..."

# Get the parent directory of where this script is.
SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ] ; do SOURCE="$(readlink "$SOURCE")"; done
DIR="$( cd -P "$( dirname "$SOURCE" )/.." && pwd )"

# Change into that dir because we expect that.
cd "$DIR"

# Delete the old dir
echo "==> Removing old directory..."
rm -rf build/pkg
mkdir -p build/pkg
#sudo chown -R 3434:3434 build

# Do a hermetic build inside a Docker container.
docker build -t mosaicnetworks/glider docker/glider
docker run --rm  \
    -u `id -u $USER` \
    -e "BUILD_TAGS=$BUILD_TAGS" \
    -v "$(pwd)":/go/src/github.com/babbleio/babble \
    -w /go/src/github.com/babbleio/babble \
    mosaicnetworks/glider ./scripts/dist_build.sh

# Add "babble" and $VERSION prefix to package name.
rm -rf ./build/dist
mkdir -p ./build/dist
for FILENAME in $(find ./build/pkg -mindepth 1 -maxdepth 1 -type f); do
  FILENAME=$(basename "$FILENAME")
	cp "./build/pkg/${FILENAME}" "./build/dist/babble_${VERSION}_${FILENAME}"
done

# Make the checksums.
pushd ./build/dist
shasum -a256 ./* > "./babble_${VERSION}_SHA256SUMS"
popd

# Done
echo
echo "==> Results:"
ls -hl ./build/dist

exit 0