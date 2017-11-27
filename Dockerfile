FROM golang:1.9.1-alpine AS BUILD

RUN apk --update --no-cache add \
    openssl \
    git \
    ca-certificates \
    && update-ca-certificates 

RUN cd /tmp \
    && wget https://github.com/coreos/etcd/releases/download/v3.2.10/etcd-v3.2.10-linux-amd64.tar.gz  \
    && tar xzf etcd-v3.2.10-linux-amd64.tar.gz \
    && mv etcd-v3.2.10-linux-amd64/etcd /usr/local/bin/etcd3 \
    && mv etcd-v3.2.10-linux-amd64/etcdctl /usr/local/bin/etcdctl3 \
    && chmod +x /usr/local/bin/etcd* \
    && rm -rf etcd-v3.2.10-linux-amd64*


COPY . /go/src/github.com/ankitforcode/ec2cluster

WORKDIR /go/src/github.com/ankitforcode/ec2cluster

# Below command will perform linting and 
# gofmt to check any missing dependency.
# Also, runs the defined tests in gofile. 
RUN go get -u github.com/golang/lint/golint \
    && find . -type f -regex '.*\.go' -exec go fmt '{}' + \
    && golint \
    && go tool vet . \
    && go get -v 

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o etcd-aws

FROM scratch

COPY --from=BUILD /usr/local/bin/etcd3 /usr/bin/etcd3
COPY --from=BUILD /usr/local/bin/etcdctl3 /usr/bin/etcdctl3
COPY --from=BUILD /go/src/github.com/ankitforcode/ec2cluster/etcd-aws /etcd-aws
VOLUME /src 

WORKDIR /src 

CMD ["/etcd-aws"]