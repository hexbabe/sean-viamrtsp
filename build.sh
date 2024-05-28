#!/bin/bash
set -e

sudo apt-get install -y libfuse2 ffmpeg pkg-config

make module
mv "$${BIN_OUTPUT_PATH}/module.tar.gz" .
