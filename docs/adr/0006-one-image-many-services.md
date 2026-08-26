# 6. One image holds every service

## Context

There are six binaries: four services, a database node, and a catalogue loader.
They share almost all of their code — HTTP plumbing, configuration, metrics,
the latency simulation, the client contracts.

The conventional layout gives each service its own image and its own registry
repository.

## Decision

Build every binary into one image. The container command selects which one
runs. One ECR repository, one tag, one thing to promote.

## Consequences

A deploy compiles the shared packages once instead of six times, and pushes one
set of layers instead of six nearly identical ones. Locally, `docker compose
up` builds once rather than six times, which is the difference between a
usable and an irritating edit-run cycle.

Every service is provably built from the same commit. Versions cannot skew
across services because there is only one version.

The image is larger than any single service needs — roughly 60MB of static
binaries rather than 10MB. On a base with no shell and no package manager that
is an acceptable trade for the build and deploy simplicity.

A change to any service rebuilds and redeploys the image for all of them. With
six services that share this much code, most changes touched several of them
anyway.

The command must be set explicitly everywhere: the compose file, the Terraform
user data, `docker run`. An image with no default command fails loudly and
immediately if that is forgotten, which is the right failure.
