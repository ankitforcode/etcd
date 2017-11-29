FROM golang:1.9.1-alpine AS BUILD

RUN apk --update --no-cache add \
    openssl \
    git \
    ca-certificates \
    && update-ca-certificates 

# This shall support both version of ETCD as things needs to be backward compatible.
ENV ETCD_VER_3 v3.2.10
ENV ETCD_VER_2 v2.3.8 

RUN cd /tmp \
    && wget https://github.com/coreos/etcd/releases/download/${ETCD_VER_3}/etcd-${ETCD_VER_3}-linux-amd64.tar.gz \
    && tar xzf etcd-${ETCD_VER_3}-linux-amd64.tar.gz \
    && mv etcd-${ETCD_VER_3}-linux-amd64/etcd /usr/local/bin/etcd3 \
    && mv etcd-${ETCD_VER_3}-linux-amd64/etcdctl /usr/local/bin/etcdctl3  \
    && wget https://github.com/coreos/etcd/releases/download/${ETCD_VER_2}/etcd-${ETCD_VER_2}-linux-amd64.tar.gz \
    && tar xzf etcd-${ETCD_VER_2}-linux-amd64.tar.gz \
    && mv etcd-${ETCD_VER_2}-linux-amd64/etcd /usr/local/bin/etcd2 \
    && mv etcd-${ETCD_VER_2}-linux-amd64/etcdctl /usr/local/bin/etcdctl2 \
    && chmod +x /usr/local/bin/etcd* \
    && rm -rf etcd-*-linux-amd64*

COPY . /go/src/github.com/ankitforcode/etcd-aws

WORKDIR /go/src/github.com/ankitforcode/etcd-aws

# Below command will perform linting and 
# gofmt to check any missing dependency.
# Also, runs the defined tests in gofile. 
RUN go get -u github.com/golang/lint/golint \
    && find . -type f -regex '.*\.go' -exec go fmt '{}' + \
    && golint \
    && go tool vet . \
    && go get -v 

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o etcd-aws

FROM alpine:latest

RUN apk --update --no-cache add \
    openssl \
    ca-certificates \
    && update-ca-certificates 

COPY --from=BUILD /usr/local/bin/etcd3 /usr/bin/etcd3
COPY --from=BUILD /usr/local/bin/etcdctl3 /usr/bin/etcdctl3
COPY --from=BUILD /usr/local/bin/etcd2 /usr/bin/etcd2
COPY --from=BUILD /usr/local/bin/etcdctl2 /usr/bin/etcdctl2
COPY --from=BUILD /go/src/github.com/ankitforcode/etcd-aws/etcd-aws /etcd-aws

VOLUME /src 

WORKDIR /src 

CMD ["/etcd-aws"]