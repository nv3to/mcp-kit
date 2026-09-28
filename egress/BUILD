go_library(
    name = "egress",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    visibility = ["PUBLIC"],
)

go_test(
    name = "egress_test",
    srcs = ["egress_test.go"],
    external = True,
    deps = [":egress"],
)

# The default dialer's check, which the public API cannot reach offline.
go_test(
    name = "address_test",
    srcs = ["address_test.go"],
    deps = [":egress"],
)

# The sources //:mcpkit_test parses to check its imports.
filegroup(
    name = "sources",
    srcs = glob(["*.go"]),
    visibility = ["PUBLIC"],
)

lint()
