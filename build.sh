#!/bin/sh
set -x
tag=$(date +%Y%m%d)
registry=spectro-dev-public
export IMG=gcr.io/$registry/${USER}/terraform-controller:$tag
make docker-build-m1-chip
make docker-push
