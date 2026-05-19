TESTS_EXT_BINARY := bin/openstack-test-tests-ext

.PHONY: tests-ext-build
tests-ext-build:
	@echo "Building OTE test extension binary..."
	@mkdir -p bin
	GOTOOLCHAIN=auto GOSUMDB=sum.golang.org go build -mod=vendor -o $(TESTS_EXT_BINARY) ./cmd/extension
	@echo "Extension binary built: $(TESTS_EXT_BINARY)"

.PHONY: extension
extension: tests-ext-build

.PHONY: clean-extension
clean-extension:
	@echo "Cleaning extension binary..."
	@rm -f $(TESTS_EXT_BINARY)

.PHONY: verify
verify:
	./hack/verify.sh
