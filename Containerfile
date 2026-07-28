# Test extension builder stage (added by ote-migration)
FROM golang:1.21 AS test-extension-builder
RUN mkdir -p /go/src/github.com/openshift/openstack-test
WORKDIR /go/src/github.com/openshift/openstack-test
COPY . .
RUN make tests-ext-build && \
    cd bin && \
    tar -czvf openstack-test-tests-ext.tar.gz openstack-test-tests-ext && \
    rm -f openstack-test-tests-ext

# Will be replaced in the CI with registry.ci.openshift.org/ocp/4.y:tools
FROM registry.access.redhat.com/ubi8/ubi

# Copy test extension binary (added by ote-migration)
COPY --from=test-extension-builder /go/src/github.com/openshift/openstack-test/bin/openstack-test-tests-ext.tar.gz /usr/bin/

USER 1000:1000
ENV LC_ALL en_US.UTF-8
