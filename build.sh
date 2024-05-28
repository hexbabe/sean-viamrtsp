#!/bin/bash
set -e

sudo apt-get install -y libfuse2 ffmpeg pkg-config

make module
mv "bin/$(shell uname -s)-$(shell uname -m)/module.tar.gz" .
