#!/bin/bash
set -e

sudo apt-get install -y libfuse2 ffmpeg pkg-config

make module
UNAME_S ?= $(uname -s)
UNAME_M ?= $(uname -m)
mv "./bin/$UNAME_S-$UNAME_M/module.tar.gz" .
