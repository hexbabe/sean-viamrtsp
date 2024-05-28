#!/bin/bash
set -e

sudo apt-get update
sudo add-apt-repository universe &&
sudo apt-get install -y libfuse2 ffmpeg pkg-config

make module
OS=$(uname -s)
mv ./bin/$OS/module.tar.gz .
