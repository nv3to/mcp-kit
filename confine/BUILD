# Package confine, and the tests that start sandboxes. Those cannot run
# inside a sandbox: the label lets a confined runner leave them out with
# `--exclude sandbox`. The test main is generated from every source given,
# whatever its build constraint, so the sources are chosen here.

go_library(
    name = "confine",
    srcs = ["backend.go", "confine.go", "verify.go"] +
           (["backend_darwin.go"] if CONFIG.OS == "darwin" else ["backend_other.go"]),
    visibility = ["PUBLIC"],
    deps = ["//:mcpkit"],
)

# The escape suite, written against the public API for every backend.
go_test(
    name = "escape_test",
    srcs = ["escape_test.go"] +
           (["suite_darwin_test.go"] if CONFIG.OS == "darwin" else ["suite_other_test.go"]),
    external = True,
    labels = ["sandbox"],
    deps = [
        ":confine",
        "//:mcpkit",
    ],
)

# What the public API cannot reach: a missing sandbox-exec, the rendering.
if CONFIG.OS == "darwin":
    go_test(
        name = "backend_test",
        srcs = ["backend_darwin_test.go"],
        labels = ["sandbox"],
        deps = [
            ":confine",
            "//:mcpkit",
        ],
    )

# Probes of what Seatbelt can express, through /usr/bin/sandbox-exec
# (docs/confinement-macos.md).
go_test(
    name = "confine_test",
    srcs = ["doc_test.go"] + (["probe_darwin_test.go"] if CONFIG.OS == "darwin" else []),
    labels = ["sandbox"],
)

# Probes of what bubblewrap can do (docs/confinement-linux.md).
go_test(
    name = "bwrap_probe_test",
    srcs = ["bwrap_probe_linux_test.go" if CONFIG.OS == "linux" else "bwrap_probe_other_test.go"],
    labels = ["sandbox"],
)

# The sources //:mcpkit_test parses to check its imports.
filegroup(
    name = "sources",
    srcs = glob(["*.go"]),
    visibility = ["PUBLIC"],
)

lint()
