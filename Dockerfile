FROM registry.ci.openshift.org/openshift/release:golang-1.19 AS builder
WORKDIR /go/src/github.com/vrutkovs/kaas
COPY . .
RUN go mod vendor && go build -o ./kaas ./cmd/kaas


FROM registry.access.redhat.com/ubi9/ubi:latest
COPY --from=builder /go/src/github.com/vrutkovs/kaas/kaas /bin/kaas
COPY --from=builder /go/src/github.com/vrutkovs/kaas/html /srv/html
RUN dnf install -y rsync
WORKDIR /srv
ENTRYPOINT ["/bin/kaas"]
