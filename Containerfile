# Will be replaced in the CI
FROM golang:1.21 AS test-extension-builder
RUN mkdir -p /go/src/github.com/openshift/openstack-test
WORKDIR /go/src/github.com/openshift/openstack-test
COPY . .
RUN make tests-ext-build && \
    cd bin && \
    tar -czvf openstack-test-test-extension.tar.gz openstack-test-tests-ext && \
    rm -f openstack-test-tests-ext

# Will be replaced in the CI with registry.ci.openshift.org/ocp/4.y:tools
FROM registry.access.redhat.com/ubi8/ubi

COPY --from=test-extension-builder /go/src/github.com/openshift/openstack-test/bin/openstack-test-test-extension.tar.gz /usr/bin/
