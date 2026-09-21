# Third-party Go: the toolchain (downloaded, hash-pinned) and every module
# from go.mod. Versions here MUST match go.mod exactly; `plz build //...`
# names any transitive module that is missing. Hashes are populated with
# `plz hash --update //third_party/go/...`, never typed by hand.
#
# Every rule carries its licence (checked against [Licences] Accept in
# .plzconfig). Licences are taken from the LICENSE file at the pinned
# version; adding a module here means adding its licence too.

# Go toolchain and stdlib: BSD-3-Clause (go.dev/LICENSE). go_toolchain and
# go_stdlib take no `licences` argument, so it is recorded here.
go_toolchain(
    name = "toolchain",
    hashes = ["ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12"],
    install_std = False,
    version = "1.27.1",
)

# The stdlib is compiled once here (hash-cached) and shared by every Go rule.
go_stdlib(
    name = "std",
    tags = [
        "netgo",
        "osusergo",
    ],
)

# The official MCP Go SDK, the one direct dependency. Upstream is moving
# from MIT to Apache-2.0; its LICENSE covers contributions under both.
go_repo(
    licences = [
        "MIT",
        "Apache-2.0",
    ],
    module = "github.com/modelcontextprotocol/go-sdk",
    version = "v1.8.0",
)

go_repo(
    licences = ["BSD-3-Clause"],
    module = "golang.org/x/sys",
    version = "v0.41.0",
)

go_repo(
    licences = ["MIT"],
    module = "github.com/google/jsonschema-go",
    version = "v0.4.3",
)

go_repo(
    licences = ["MIT"],
    module = "github.com/segmentio/encoding",
    version = "v0.5.4",
)

go_repo(
    licences = ["MIT"],
    module = "github.com/segmentio/asm",
    version = "v1.1.3",
)

go_repo(
    licences = ["BSD-3-Clause"],
    module = "github.com/yosida95/uritemplate/v3",
    version = "v3.0.2",
)

go_repo(
    licences = ["BSD-3-Clause"],
    module = "golang.org/x/oauth2",
    version = "v0.35.0",
)

go_repo(
    licences = ["BSD-3-Clause"],
    module = "golang.org/x/sync",
    version = "v0.20.0",
)

go_repo(
    licences = ["BSD-3-Clause"],
    module = "golang.org/x/time",
    version = "v0.15.0",
)
