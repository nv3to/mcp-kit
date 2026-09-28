go_library(
    name = "budget",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    visibility = ["PUBLIC"],
    deps = ["//:mcpkit"],
)

# The sources //:mcpkit_test parses to check its imports.
filegroup(
    name = "sources",
    srcs = glob(["*.go"]),
    visibility = ["PUBLIC"],
)

go_test(
    name = "budget_test",
    srcs = glob(["*_test.go"]),
    external = True,
    data = glob(["testdata/*"]),
    deps = [
        ":budget",
        "//:mcpkit",
    ],
)

lint()
