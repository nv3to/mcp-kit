# Root package: go.mod for the Go plugin, and mcpkit itself, the MCP server
# library over the official Go SDK. It depends on the SDK and nothing else.
filegroup(
    name = "gomod",
    srcs = ["go.mod"],
    visibility = ["PUBLIC"],
)

go_library(
    name = "mcpkit",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    visibility = ["PUBLIC"],
    deps = ["///third_party/go/github.com_modelcontextprotocol_go-sdk//mcp"],
)

# The library sources the import test parses; mcptest, budget and egress
# export their own. The test files are the test target's own srcs and are
# not repeated as data.
filegroup(
    name = "sources",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    visibility = ["PUBLIC"],
)

go_test(
    name = "mcpkit_test",
    srcs = glob(["*_test.go"]),
    external = True,
    data = [
        ":sources",
        "//budget:sources",
        "//egress:sources",
        "//mcptest:sources",
    ],
    deps = [
        ":mcpkit",
        "//mcptest",
        "///third_party/go/github.com_modelcontextprotocol_go-sdk//mcp",
    ],
)

lint()

# Repo-wide lint: go vet, and the guard that every source is in some
# package's lint() and that package is listed here (github.com/nv3to/lint-rules).
repo_lint(
    name = "vet",
    packages = [
        "//",
        "//budget",
        "//confine",
        "//egress",
        "//mcptest",
    ],
)
