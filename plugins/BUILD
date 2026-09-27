# Please plugins, pinned by version. Hashes are populated by
# `plz hash --update //plugins:all` after the first successful fetch; never
# hand-edit a hash here.
#
# Licence: please-build/go-rules is Apache-2.0 (LICENSE at the pinned
# revision). plugin_repo is a Please built-in that takes no `licences`
# argument, so it is recorded here.
plugin_repo(
    name = "go",
    hashes = ["f47fbc2210724bd6bf8d2754290450709c7bf23558e1f3dce96398948cb45237"],
    owner = "please-build",
    plugin = "go-rules",
    revision = "v1.33.0",
)

# lint-rules lives in-tree until it moves to its own repository; then this
# becomes a plugin_repo like the one above and nothing else changes.
subrepo(
    name = "lint",
    path = "plugins/lint-rules",
    plugin = True,
)
