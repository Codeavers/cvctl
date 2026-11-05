FROM alpine:3.22

# Pinned so a rebuild cannot change what reads existing backups. Alpine drops
# old revisions from the branch: a failed build here means update the pin, and
# the tag in kinds/sqlite.yml when the major changes.
RUN apk add --no-cache sqlite=3.49.2-r1
