#!/usr/bin/env bash
# Signs the image the Release workflow pushed and writes the release notes section
# that says where it was published, under which tags and with which digest.
#
# One buildx build pushes the same multi-arch index to every registry, so all
# of them hold one digest. It is signed by digest in each registry with cosign
# keyless signing: the certificate names this repository's workflow, the
# signature is logged in Rekor, and no key exists to keep or leak.
#
#   METADATA  buildx --metadata-file of the build
#   IMAGES    space separated image repositories the build pushed to
#   TAGS      space separated tags the build pushed, the version first
#   NOTES     file to write the release notes section to
# Needs cosign and jq, a login to every registry, and id-token: write.
set -euo pipefail

digest=$(jq -r '."containerimage.digest" // empty' "$METADATA")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "ERROR: no image digest in $METADATA" >&2; exit 1; }

read -r -a images <<<"$IMAGES"
read -r -a tags <<<"$TAGS"
version="${tags[0]}"
identity="^https://github\\.com/${GITHUB_REPOSITORY}/"
issuer="https://token.actions.githubusercontent.com"

for image in "${images[@]}"; do
  # A registry that holds another digest under the version tag would get a
  # signature for an image nobody pulls: fail instead.
  pushed=$(docker buildx imagetools inspect "$image:$version" --format '{{.Manifest.Digest}}')
  [ "$pushed" = "$digest" ] || { echo "ERROR: $image:$version is $pushed, the build pushed $digest" >&2; exit 1; }
  cosign sign --yes "$image@$digest"
done
for image in "${images[@]}"; do
  cosign verify --certificate-oidc-issuer "$issuer" --certificate-identity-regexp "$identity" "$image@$digest" >/dev/null
done

tag_list=$(printf '`%s`, ' "${tags[@]}")
{
  echo "## Container images"
  echo
  echo "One multi-arch image (linux/amd64, linux/arm64), signed with cosign, in every registry:"
  echo
  echo "| Image | Tags | Digest |"
  echo "| --- | --- | --- |"
  for image in "${images[@]}"; do
    echo "| \`$image\` | ${tag_list%, } | \`$digest\` |"
  done
  echo
  echo "Verify the signature, and pull by digest to get exactly this image:"
  echo
  echo '```bash'
  echo "cosign verify --certificate-oidc-issuer $issuer \\"
  echo "  --certificate-identity-regexp '$identity' \\"
  echo "  ${images[0]}@$digest"
  echo '```'
  echo
} > "$NOTES"
cat "$NOTES" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
echo "digest=$digest" >> "${GITHUB_OUTPUT:-/dev/null}"
