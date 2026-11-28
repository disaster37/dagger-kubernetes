FROM alpine:3.20

COPY app /app

# Stamp the COPY layer: re-running this RUN proves the layer was rebuilt.
RUN head -c16 /dev/urandom | sha256sum > /copy-stamp

# A bind-mounted file outside the COPY subtree: changing it must NOT rebuild
# the COPY layer, and changing app/main.txt must.
RUN --mount=type=bind,source=app/main.txt,target=/mnt/main.txt \
    cat /mnt/main.txt >/dev/null && head -c16 /dev/urandom | sha256sum > /mount-stamp

CMD ["cat", "/copy-stamp", "/mount-stamp"]
