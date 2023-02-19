#!/bin/sh
set -x
tag=$(date +%Y%m%d)
export IMG=gcr.io/spectro-dev-public/${USER}/terraform-controller:$tag
make docker-build-m1-chip
make docker-push

# tag=$(date +%Y%m%d)
# make docker-build && docker tag docker.io/oamdev/terraform-controller:0.2.8 lochan2120/terraform-controller:$tag && docker push lochan2120/terraform-controller:$tag
