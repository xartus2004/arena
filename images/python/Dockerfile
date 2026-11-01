# Sandbox image for the example environments.
#
# The upstream python:alpine image ships no .pyc files. arena sandboxes have
# a read-only root filesystem, so without this every Python process would
# recompile the stdlib it imports (~140 ms for `import json`) and could
# never cache the result. Precompiling once at build time fixes that.
FROM python:3.12-alpine
RUN python -m compileall -q -j 0 /usr/local/lib/python3.12
