go_library(
    name = "mcptest",
    srcs = ["mcptest.go"],
    visibility = ["PUBLIC"],
    deps = [
        "//:mcpkit",
        "///third_party/go/github.com_modelcontextprotocol_go-sdk//mcp",
    ],
)

# The sources //:mcpkit_test parses to check its imports.
filegroup(
    name = "sources",
    srcs = glob(["*.go"]),
    visibility = ["PUBLIC"],
)

lint()
