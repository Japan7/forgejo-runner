FROM golang:1.19 AS builder
WORKDIR /src
COPY . .
RUN make build

FROM registry.access.redhat.com/ubi9/ubi-minimal AS runtime
WORKDIR /app
COPY --from=builder /src/forgejo-runner .
ENTRYPOINT [ "/app/forgejo-runner" ]
