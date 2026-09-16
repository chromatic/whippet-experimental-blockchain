#!/usr/bin/env bash
#
# release.sh - Whippet Core release management.
#
# Supersedes the ad-hoc release machinery that used to live at the repo root
# (bump-version-<v>.sh, run-gitian-build.sh) and automates the mechanical parts
# of RELEASE_CHECKLIST_*.md / RELEASE_FINAL_TODO.md.
#
# Three phases, runnable together or independently:
#
#   bump         Set the target version everywhere it actually lives.
#   check-notes  Validate doc/release-notes/release-notes-<version>.md is real.
#   gitian       Launch the deterministic build via contrib/gitian-build.sh.
#
# Usage:
#   contrib/devtools/release.sh [options] <subcommand> <version>
#   contrib/devtools/release.sh [options] <version>            # implies "all"
#
# Examples:
#   contrib/devtools/release.sh --dry-run bump 1.3.0
#   contrib/devtools/release.sh check-notes 1.2.0
#   contrib/devtools/release.sh gitian --platforms l --signer me --yes 1.3.0
#
# Authorization is split by consequence, and each level must be given
# explicitly - none of them implies another:
#
#   (default)    read-only checks; refuses to do anything expensive.
#   --yes        authorizes the local, expensive, hard-to-interrupt work:
#                the gitian build, and (for "all") the local commit + tag.
#   --push-tag   authorizes the one irreversible, publishing action:
#                "git push <remote> v<version>". Never implied by --yes.
#
# The "bump" and "check-notes" subcommands never commit, tag, or push.
# The "all" subcommand DOES create a local commit and a local tag, because the
# gitian step can only build a tag that already exists; see phase_tag.
#
# Deliberately NOT automated: doc/man regeneration (it must execute freshly
# built binaries) and pushing anything without --push-tag.

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults / globals
# ---------------------------------------------------------------------------

SCRIPT_NAME=$(basename -- "$0")

DRY_RUN=false
ALLOW_DIRTY=false
ALLOW_EXISTING_TAG=false
ASSUME_YES=false
ROOT=""
VERSION=""
SUBCOMMAND=""

# Set whenever a dry run encounters something that would be fatal for real, so
# that "--dry-run" cannot report success for a run that would not succeed.
DRY_RUN_WOULD_FAIL=false

# bump options
RELEASE_DATE=""
VERSION_BUILD=0
IS_RELEASE=true

# tag options (used by "all", which must hand an existing tag to gitian)
SIGN_TAG=true
# Set by phase_tag in a dry run: the tag would exist by the time phase_gitian
# runs for real, so phase_gitian must not report its absence as a failure.
TAG_PENDING=false

# gitian options
PLATFORMS="lwx"
SIGNER="${SIGNER:-}"
NO_SIGN=false
REMOTE="upstream"
GITIAN_SETUP=false
VIRT=""                 # "", "--docker" or "--lxc"
# Pushing publishes the tag and cannot be undone politely. It is NOT covered by
# --yes (which only confirms local, expensive work); it needs --push-tag.
PUSH_TAG=false
SKIP_DESCRIPTOR_CHECK=false
GITIAN_JOBS=""
GITIAN_MEM=""

# release-notes thresholds (calibrated against the real 1.2.0 notes, which are
# ~6.8 KiB / ~150 non-blank lines; the smallest genuine notes in-tree, 1.7.0,
# are ~2.2 KiB, so the floor sits below that and well above any stub).
NOTES_MIN_BYTES=1200
NOTES_MIN_LINES=25
NOTES_MIN_VERSION_MENTIONS=3

# accumulated results
declare -a CHANGED=()
declare -a UNCHANGED=()
declare -a WARNINGS=()

# Files actually written to disk by the bump, appended only AFTER the write
# succeeds, so that an abort can report the true on-disk state.
declare -a WRITTEN_FILES=()
BUMP_ACTIVE=false

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
    C_RED=$'\033[31m'; C_YEL=$'\033[33m'; C_GRN=$'\033[32m'
    C_BLD=$'\033[1m'; C_OFF=$'\033[0m'
else
    C_RED=""; C_YEL=""; C_GRN=""; C_BLD=""; C_OFF=""
fi

info()  { printf '%s\n' "$*"; }
head1() { printf '\n%s==> %s%s\n' "$C_BLD" "$*" "$C_OFF"; }
ok()    { printf '  %sok%s   %s\n' "$C_GRN" "$C_OFF" "$*"; }
warn()  { printf '  %swarn%s %s\n' "$C_YEL" "$C_OFF" "$*"; WARNINGS+=("$*"); }
fail()  { printf '  %sFAIL%s %s\n' "$C_RED" "$C_OFF" "$*"; }
die()   { printf '%s%s: error:%s %s\n' "$C_RED" "$SCRIPT_NAME" "$C_OFF" "$*" >&2; exit 1; }

usage() {
    cat <<EOF
$SCRIPT_NAME - Whippet Core release management

Usage:
  $SCRIPT_NAME [options] <subcommand> <version>
  $SCRIPT_NAME [options] <version>              same as: all <version>

Version format:
  MAJOR.MINOR.PATCH, optionally followed by a pre-release suffix in one of the
  forms this project actually tags: -alpha-N, -beta-N, -rc-N (any casing, e.g.
  1.7.0-RC-1, 1.14.0-beta-1). A suffixed version implies --pre-release.

Subcommands:
  bump          Rewrite the version number in every file that carries it.
                Never commits, tags, or pushes.
  check-notes   Pre-flight check of doc/release-notes/release-notes-<v>.md.
  gitian        Launch the deterministic gitian build for <v>.
  all           bump, check-notes, commit + tag, then gitian.
                Needs --yes: it creates a local commit and a local tag,
                because gitian can only build a tag that already exists.

Global options:
  -n, --dry-run          Show every change without making any. Read-only.
                         Exits non-zero if the real run would have failed.
      --root DIR         Operate on DIR instead of this checkout (for testing).
      --allow-dirty      Proceed even though the working tree has changes.
      --allow-existing-tag
                         Proceed even though tag v<version> already exists.
  -y, --yes              Authorize the expensive LOCAL work: the multi-hour
                         gitian build, and "all"'s local commit + tag.
                         It does NOT authorize any push; see --push-tag.
  -h, --help             This message.

Publishing (separate on purpose - the only irreversible step):
      --push-tag         Authorize "git push <remote> v<version>" before the
                         gitian build. Off by default. Not implied by --yes.
      --no-push-tag      Explicitly keep pushing off (the default).

bump options:
      --release-date D   _CURRENT_RELEASE_DATE value (default: today, UTC).
      --build N          _CLIENT_VERSION_BUILD value (default: 0).
      --pre-release      Set *_IS_RELEASE to false instead of true.

tag options (subcommand "all"):
      --no-sign-tag      Create an annotated tag (-a) instead of a signed one.

gitian options:
      --platforms lwx    Subset to build: l=linux, w=windows, x=macos.
                         Also accepts "linux,win,osx". Default: lwx.
      --signer NAME      GPG signer id for gsign (or set \$SIGNER).
      --no-sign          Build without signing.
      --remote NAME      Git remote holding the release tag. Default: upstream.
      --setup            Pass --setup to contrib/gitian-build.sh first.
      --docker           Use Docker virtualization.
      --lxc              Use LXC virtualization.
  -j N                   gbuild processes.
  -m N                   gbuild memory (MiB).
      --skip-descriptor-check
                         Skip verifying the tagged descriptors pin v<version>.

Left manual on purpose:
  * git push              - needs --push-tag, every time, explicitly.
  * doc/man/*.1           - regenerated by contrib/devtools/gen-manpages.sh,
                            which executes the freshly built binaries.
  * contrib/debian/changelog, contrib/rpm/bitcoin.spec - unmaintained upstream
                            packaging metadata; reported, never rewritten.
EOF
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

parse_args() {
    local positional=()
    while (( $# )); do
        case "$1" in
            -n|--dry-run)            DRY_RUN=true ;;
            --root)                  ROOT=${2:?--root needs a directory}; shift ;;
            --root=*)                ROOT=${1#*=} ;;
            --allow-dirty)           ALLOW_DIRTY=true ;;
            --allow-existing-tag)    ALLOW_EXISTING_TAG=true ;;
            -y|--yes)                ASSUME_YES=true ;;
            -h|--help)               usage; exit 0 ;;

            --release-date)          RELEASE_DATE=${2:?--release-date needs a value}; shift ;;
            --release-date=*)        RELEASE_DATE=${1#*=} ;;
            --build)                 VERSION_BUILD=${2:?--build needs a value}; shift ;;
            --build=*)               VERSION_BUILD=${1#*=} ;;
            --pre-release)           IS_RELEASE=false ;;

            --no-sign-tag)           SIGN_TAG=false ;;

            --platforms)             PLATFORMS=${2:?--platforms needs a value}; shift ;;
            --platforms=*)           PLATFORMS=${1#*=} ;;
            --signer)                SIGNER=${2:?--signer needs a value}; shift ;;
            --signer=*)              SIGNER=${1#*=} ;;
            --no-sign)               NO_SIGN=true ;;
            --remote)                REMOTE=${2:?--remote needs a value}; shift ;;
            --remote=*)              REMOTE=${1#*=} ;;
            --push-tag|--push)       PUSH_TAG=true ;;
            --no-push-tag)           PUSH_TAG=false ;;
            --setup)                 GITIAN_SETUP=true ;;
            --docker)                VIRT="--docker" ;;
            --lxc)                   VIRT="--lxc" ;;
            -j)                      GITIAN_JOBS=${2:?-j needs a value}; shift ;;
            -m)                      GITIAN_MEM=${2:?-m needs a value}; shift ;;
            --skip-descriptor-check) SKIP_DESCRIPTOR_CHECK=true ;;

            # End of options. Everything after it is positional, including a
            # literal "-something". "break" (not "shift") is essential: after
            # draining $@ there is nothing left for the loop's trailing shift,
            # and under "set -e" that failing shift would abort the script -
            # a bare "--" used to exit 1 with no message at all.
            --) shift; while (( $# )); do positional+=("$1"); shift; done; break ;;
            -*) die "unknown option: $1 (try --help)" ;;
            *)  positional+=("$1") ;;
        esac
        shift
    done

    case ${#positional[@]} in
        1) SUBCOMMAND=all;             VERSION=${positional[0]} ;;
        2) SUBCOMMAND=${positional[0]}; VERSION=${positional[1]} ;;
        0) usage >&2; exit 1 ;;
        *) die "too many arguments: ${positional[*]}" ;;
    esac

    case "$SUBCOMMAND" in
        bump|check-notes|gitian|all) ;;
        *) die "unknown subcommand: $SUBCOMMAND (try --help)" ;;
    esac
}

# A typo'd version silently written into a dozen files is the exact failure
# this script exists to prevent, so be strict - but strict about the shapes
# this project actually tags, not an idealised subset of them.
#
# "git tag --list" on this repo contains, besides plain MAJOR.MINOR.PATCH:
#   v1.7.0-RC-1  v1.7.0-Alpha-1  v1.7.0-Beta-2  v1.8.0-beta-1
#   v1.10.1-alpha-1  v1.14-beta-1  v1.14-rc-1  v1.10.0-RC-1
# so -alpha-N / -beta-N / -rc-N in any casing is accepted. (The two-component
# "v1.14-beta-1" shape is still refused: configure.ac needs three numeric
# components, and guessing the third is exactly the kind of silent invention
# this script must not do.)
#
# Historic oddities that are NOT accepted, deliberately: "1.4alpha",
# "1.5alpha2" (no separators - unparseable), and "v1.10-rc-1-dogeparty"
# (a fork label, not a Whippet release).
validate_version() {
    local re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|Alpha|ALPHA|beta|Beta|BETA|rc|Rc|RC)-([1-9][0-9]*))?$'
    [[ $VERSION =~ $re ]] || die "invalid version '$VERSION': expected MAJOR.MINOR.PATCH \
with an optional -alpha-N / -beta-N / -rc-N suffix (no leading zeros, no 'v' prefix)"
    # capture immediately: any later [[ =~ ]] clobbers BASH_REMATCH
    V_MAJOR=${BASH_REMATCH[1]}
    V_MINOR=${BASH_REMATCH[2]}
    V_REVISION=${BASH_REMATCH[3]}
    V_SUFFIX=${BASH_REMATCH[4]}          # "" or "-RC-1"
    VERSION_CORE="$V_MAJOR.$V_MINOR.$V_REVISION"

    # configure.ac's *_IS_RELEASE must not claim a pre-release is final.
    if [[ -n $V_SUFFIX && $IS_RELEASE == true ]]; then
        IS_RELEASE=false
        warn "'$VERSION' carries a pre-release suffix; setting *_IS_RELEASE=false (implied --pre-release)"
    fi

    [[ $VERSION_BUILD =~ ^[0-9]+$ ]] || die "invalid --build '$VERSION_BUILD': expected an integer"
    if [[ -n $RELEASE_DATE && ! $RELEASE_DATE =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
        die "invalid --release-date '$RELEASE_DATE': expected YYYY-MM-DD"
    fi
}

resolve_root() {
    if [[ -z $ROOT ]]; then
        local here
        here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
        ROOT=$(cd "$here" && git rev-parse --show-toplevel 2>/dev/null || true)
        [[ -n $ROOT ]] || ROOT=$(cd "$here/../.." && pwd)
    fi
    [[ -d $ROOT ]] || die "--root: not a directory: $ROOT"
    ROOT=$(cd "$ROOT" && pwd)
    [[ -f $ROOT/configure.ac ]] || die "$ROOT does not look like a Whippet checkout (no configure.ac)"
}

# ---------------------------------------------------------------------------
# Guards
# ---------------------------------------------------------------------------

in_git_repo() { git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; }

# In dry-run nothing is written, so guard violations do not abort: you should
# always be able to preview safely. They are still recorded as "the real run
# would have failed here", because a dry run that exits 0 while the real run
# would exit 1 is worse than having no dry run at all.
guard() {
    local msg=$1 override=$2
    if [[ $override == true ]]; then
        warn "$msg (overridden)"
    elif [[ $DRY_RUN == true ]]; then
        warn "$msg (dry-run: not fatal, would refuse for real)"
        DRY_RUN_WOULD_FAIL=true
    else
        fail "$msg"
        die "refusing to continue; re-run with --dry-run to preview, or use the documented override"
    fi
}

check_clean_tree() {
    in_git_repo || { warn "not a git repository: skipping clean-tree check"; return; }
    if [[ -n $(git -C "$ROOT" status --porcelain --untracked-files=no) ]]; then
        guard "working tree is dirty (tracked files modified); use --allow-dirty to proceed" "$ALLOW_DIRTY"
    else
        ok "working tree is clean"
    fi
}

check_tag_absent() {
    in_git_repo || return 0
    if git -C "$ROOT" rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null; then
        guard "tag v$VERSION already exists; use --allow-existing-tag to proceed" "$ALLOW_EXISTING_TAG"
    else
        ok "tag v$VERSION does not exist yet"
    fi
}

# ---------------------------------------------------------------------------
# Edit engine
#
# Every edit is scoped to one file and one anchored pattern, and fails loudly
# when the pattern does not match exactly once. No repo-wide sed.
# ---------------------------------------------------------------------------

# edit_line <relpath> <label> <anchor-regex> <new full line>
edit_line() {
    local rel=$1 label=$2 re=$3 new=$4
    local file="$ROOT/$rel"

    [[ -f $file ]] || die "$rel: file not found (expected for: $label)"

    local matches count
    matches=$(grep -nE "$re" "$file" || true)
    count=$(printf '%s' "$matches" | grep -c . || true)
    if [[ $count -ne 1 ]]; then
        die "$rel: expected exactly one line matching /$re/ for '$label', found $count.
The file layout changed; fix this script rather than letting the bump miss a location."
    fi

    local lineno cur
    lineno=${matches%%:*}
    cur=${matches#*:}

    if [[ $cur == "$new" ]]; then
        UNCHANGED+=("$rel:$lineno  $label (already '$new')")
        return 0
    fi

    local exact
    exact=$(grep -cFx -- "$cur" "$file" || true)
    [[ $exact -eq 1 ]] || die "$rel: line '$cur' occurs $exact times; refusing an ambiguous edit"

    if [[ $DRY_RUN == false ]]; then
        local tmp
        tmp=$(mktemp "${TMPDIR:-/tmp}/release.XXXXXX")
        # NOT "awk -v old=...": -v assignments are escape-processed, so a value
        # containing a backslash (perfectly legal in these files) arrives at
        # awk mangled - "\t" would become a tab and the comparison would
        # silently never match. ENVIRON is passed through verbatim.
        RELEASE_OLD_LINE=$cur RELEASE_NEW_LINE=$new awk '
            BEGIN { old = ENVIRON["RELEASE_OLD_LINE"]; new = ENVIRON["RELEASE_NEW_LINE"] }
            $0 == old { print new; next }
            { print }
        ' "$file" >"$tmp"
        # Belt and braces: the rewrite must have produced exactly the change we
        # promised, or we stop before touching the original.
        if ! grep -qFx -- "$new" "$tmp"; then
            rm -f "$tmp"
            die "$rel: rewrite of '$label' produced no matching line; refusing to write"
        fi
        cat "$tmp" >"$file"        # preserve mode/ownership of the original
        rm -f "$tmp"
        # Recorded only after the bytes are on disk, so the abort report below
        # describes the real state of the tree and never over-reports.
        WRITTEN_FILES+=("$rel")
    fi

    CHANGED+=("$rel:$lineno  $label
      - $cur
      + $new")
}

# ---------------------------------------------------------------------------
# Abort reporting
#
# The 1.1.0 release bug was a half-applied version bump: some files carried the
# new version, some the old, and nobody knew which. An abort in the middle of
# phase_bump recreates exactly that state, so it must never be silent about
# what is already on disk.
# ---------------------------------------------------------------------------

bump_abort_report() {
    local rc=$?
    [[ $BUMP_ACTIVE == true ]] || return 0
    BUMP_ACTIVE=false
    (( rc != 0 )) || return 0

    {
        printf '\n%s%s: BUMP ABORTED PART-WAY%s\n' "$C_RED" "$SCRIPT_NAME" "$C_OFF"
        if [[ $DRY_RUN == true ]]; then
            printf '  dry-run: nothing was written; the tree is untouched.\n'
            return 0
        fi
        if (( ${#WRITTEN_FILES[@]} == 0 )); then
            printf '  No file was modified before the abort; the tree is untouched.\n'
            return 0
        fi

        local uniq
        uniq=$(printf '%s\n' "${WRITTEN_FILES[@]}" | sort -u)

        printf '  The tree is now INCONSISTENT: the following file(s) were already\n'
        printf '  rewritten to %s, while every location after the failure still\n' "$VERSION"
        printf '  carries the old version. Do not commit or tag in this state.\n\n'
        printf '  modified on disk (%s edit(s) across %s file(s)):\n' \
            "${#WRITTEN_FILES[@]}" "$(printf '%s\n' "$uniq" | grep -c .)"
        printf '%s\n' "$uniq" | sed 's/^/    /'
        printf '\n  exact edits applied before the abort:\n'
        local c
        for c in "${CHANGED[@]}"; do
            printf '    %s\n' "$c"
        done
        printf '\n  To recover, either revert them:\n'
        printf '    git -C %s checkout -- %s\n' "$ROOT" "$(printf '%s\n' "$uniq" | tr '\n' ' ')"
        printf '  or fix the cause and re-run the same bump - it is idempotent and\n'
        printf '  will report the already-correct locations as "already correct".\n'
    } >&2
    return 0
}

# ---------------------------------------------------------------------------
# Phase 1: version bump
# ---------------------------------------------------------------------------

current_version_from_configure_ac() {
    local f="$ROOT/configure.ac" maj min rev
    maj=$(sed -n 's/^define(_CLIENT_VERSION_MAJOR, *\([0-9][0-9]*\))$/\1/p' "$f")
    min=$(sed -n 's/^define(_CLIENT_VERSION_MINOR, *\([0-9][0-9]*\))$/\1/p' "$f")
    rev=$(sed -n 's/^define(_CLIENT_VERSION_REVISION, *\([0-9][0-9]*\))$/\1/p' "$f")
    [[ -n $maj && -n $min && -n $rev ]] || \
        die "could not read the current version from configure.ac (_CLIENT_VERSION_* macros not found)"
    printf '%s.%s.%s\n' "$maj" "$min" "$rev"
}

phase_bump() {
    head1 "Phase 1: version bump -> $VERSION"

    # Armed for the whole phase: any die() from here on runs bump_abort_report,
    # which names every file already rewritten.
    BUMP_ACTIVE=true
    trap bump_abort_report EXIT

    CURRENT_VERSION=$(current_version_from_configure_ac)
    info "  current version (from configure.ac): $CURRENT_VERSION"
    info "  target version:                      $VERSION"
    if [[ $CURRENT_VERSION == "$VERSION" ]]; then
        info "  (already at target; the bump is a no-op unless other files lag behind)"
    fi

    local date_value=${RELEASE_DATE:-$(date -u +%Y-%m-%d)}
    info "  release date:                        $date_value"
    info "  build number:                        $VERSION_BUILD"
    info "  is-release flag:                     $IS_RELEASE"
    info ""

    # --- configure.ac: separate MAJOR/MINOR/REVISION/BUILD macros ---
    edit_line configure.ac "_CLIENT_VERSION_MAJOR" \
        '^define\(_CLIENT_VERSION_MAJOR, *[0-9]+\)$' \
        "define(_CLIENT_VERSION_MAJOR, $V_MAJOR)"
    edit_line configure.ac "_CLIENT_VERSION_MINOR" \
        '^define\(_CLIENT_VERSION_MINOR, *[0-9]+\)$' \
        "define(_CLIENT_VERSION_MINOR, $V_MINOR)"
    edit_line configure.ac "_CLIENT_VERSION_REVISION" \
        '^define\(_CLIENT_VERSION_REVISION, *[0-9]+\)$' \
        "define(_CLIENT_VERSION_REVISION, $V_REVISION)"
    edit_line configure.ac "_CLIENT_VERSION_BUILD" \
        '^define\(_CLIENT_VERSION_BUILD, *[0-9]+\)$' \
        "define(_CLIENT_VERSION_BUILD, $VERSION_BUILD)"
    edit_line configure.ac "_CLIENT_VERSION_IS_RELEASE" \
        '^define\(_CLIENT_VERSION_IS_RELEASE, *(true|false)\)$' \
        "define(_CLIENT_VERSION_IS_RELEASE, $IS_RELEASE)"
    edit_line configure.ac "_CURRENT_RELEASE_DATE" \
        '^define\(_CURRENT_RELEASE_DATE,\[\[[0-9]{4}-[0-9]{2}-[0-9]{2}\]\]\)$' \
        "define(_CURRENT_RELEASE_DATE,[[$date_value]])"

    # --- src/clientversion.h: mirrors configure.ac for non-autotools builds ---
    edit_line src/clientversion.h "CLIENT_VERSION_MAJOR" \
        '^#define CLIENT_VERSION_MAJOR [0-9]+$' \
        "#define CLIENT_VERSION_MAJOR $V_MAJOR"
    edit_line src/clientversion.h "CLIENT_VERSION_MINOR" \
        '^#define CLIENT_VERSION_MINOR [0-9]+$' \
        "#define CLIENT_VERSION_MINOR $V_MINOR"
    edit_line src/clientversion.h "CLIENT_VERSION_REVISION" \
        '^#define CLIENT_VERSION_REVISION [0-9]+$' \
        "#define CLIENT_VERSION_REVISION $V_REVISION"
    edit_line src/clientversion.h "CLIENT_VERSION_BUILD" \
        '^#define CLIENT_VERSION_BUILD [0-9]+$' \
        "#define CLIENT_VERSION_BUILD $VERSION_BUILD"
    edit_line src/clientversion.h "CLIENT_VERSION_IS_RELEASE" \
        '^#define CLIENT_VERSION_IS_RELEASE (true|false)$' \
        "#define CLIENT_VERSION_IS_RELEASE $IS_RELEASE"

    # --- doc/Doxyfile ---
    edit_line doc/Doxyfile "PROJECT_NUMBER" \
        '^PROJECT_NUMBER +=' \
        "PROJECT_NUMBER         = $VERSION"

    # --- gitian descriptors: the commit each deterministic build checks out ---
    #
    # This is the location the 1.1.0 release got wrong: the tag was created
    # before the descriptors were updated, so gitian happily built v1.0.0
    # sources and labelled them 1.1.0. Bump these BEFORE tagging.
    local d
    for d in linux win osx; do
        edit_line "gitian-descriptors/gitian-$d.yml" "gitian $d commit pin" \
            '^ +"commit": ".*"$' \
            "  \"commit\": \"v$VERSION\""

        # The descriptor's own name. gbuild stamps it into the build result
        # (gitian-output/*.assert / the cache directory), so leaving it at the
        # inherited "1.14" makes every build artefact self-report a version
        # nobody is releasing. Upstream's convention is MAJOR.MINOR, which is
        # kept here; the pre-release suffix is not part of it.
        edit_line "gitian-descriptors/gitian-$d.yml" "gitian $d descriptor name" \
            '^name: ".*"$' \
            "name: \"whippet-$d-$V_MAJOR.$V_MINOR\""
    done

    # --- contrib/snap/snapcraft.yaml ---
    edit_line contrib/snap/snapcraft.yaml "snap version" \
        "^version: '.*'$" \
        "version: '$VERSION'"

    report_bump_summary
    report_derived_and_manual

    # Completed cleanly: disarm the abort report.
    BUMP_ACTIVE=false
    trap - EXIT
}

report_bump_summary() {
    head1 "Version bump summary"
    if (( ${#CHANGED[@]} == 0 )); then
        ok "no changes needed - every tracked version location already reads $VERSION"
    else
        local prefix="changed"
        [[ $DRY_RUN == true ]] && prefix="would change"
        info "  ${#CHANGED[@]} location(s) $prefix:"
        local c
        for c in "${CHANGED[@]}"; do
            info "    $c"
        done
    fi
    if (( ${#UNCHANGED[@]} )); then
        info ""
        info "  ${#UNCHANGED[@]} location(s) already correct:"
        local u
        for u in "${UNCHANGED[@]}"; do
            info "    $u"
        done
    fi

    info ""
    info "  files touched:"
    local files
    files=$(printf '%s\n' "${CHANGED[@]}" | sed -n 's/^\([^: ]*\):[0-9].*/    \1/p' | sort -u)
    if [[ -n $files ]]; then
        info "$files"
    else
        info "    (none)"
    fi
}

# Locations that carry a version but are derived, generated, or unmaintained.
# We report them instead of rewriting them - silently editing generated files
# is how you get a build that disagrees with itself.
report_derived_and_manual() {
    head1 "Derived locations (no action needed)"
    info "  share/setup.nsi.in         -> @CLIENT_VERSION_*@, substituted by configure"
    info "  share/qt/Info.plist.in     -> @CLIENT_VERSION_*@, substituted by configure"
    info "  src/qt/res/bitcoin-qt-res.rc -> #includes clientversion.h"
    info "  src/clientversion.cpp      -> formats the CLIENT_VERSION_* macros"

    head1 "Manual follow-ups (not automated, on purpose)"

    local stale
    stale=$(grep -hoE 'v[0-9]+\.[0-9]+\.[0-9]+(\.[0-9]+)?' "$ROOT"/doc/man/*.1 2>/dev/null | sort -u | tr '\n' ' ' || true)
    if [[ -n $stale && $stale != "v$VERSION " ]]; then
        warn "doc/man/*.1 still advertise: ${stale% }"
        info "        These are help2man output. Regenerate AFTER building:"
        info "          contrib/devtools/gen-manpages.sh"
    else
        ok "doc/man/*.1 look current"
    fi

    if grep -qE '^bitcoin \(' "$ROOT/contrib/debian/changelog" 2>/dev/null; then
        warn "contrib/debian/changelog is still upstream Bitcoin packaging (not Whippet); left alone"
    fi
    if [[ -f $ROOT/contrib/rpm/bitcoin.spec ]]; then
        local rpmver
        rpmver=$(sed -n 's/^Version:[[:space:]]*//p' "$ROOT/contrib/rpm/bitcoin.spec" | head -1)
        [[ $rpmver == "$VERSION" ]] || \
            warn "contrib/rpm/bitcoin.spec Version is '$rpmver' (upstream Bitcoin packaging); left alone"
    fi

    local proto
    proto=$(sed -n 's/^static const int PROTOCOL_VERSION = \([0-9]*\);.*/\1/p' "$ROOT/src/version.h" | head -1)
    info "  src/version.h PROTOCOL_VERSION is $proto - bump by hand only when the"
    info "        p2p protocol actually changed; it is not tied to the client version."

    info ""
    info "  Next steps (review the diff yourself before any of them):"
    info "    git diff"
    info "    make -j\$(nproc) && make check"
    info "    git add -u && git commit -m 'version: bump to $VERSION'"
    info "    git tag -s v$VERSION -m 'Whippet Core $VERSION'"
    info "    $SCRIPT_NAME gitian --yes --push-tag $VERSION"
    info ""
    info "  --push-tag is what authorizes 'git push $REMOTE v$VERSION'; --yes"
    info "        only authorizes the build. Omit --push-tag if the tag is already"
    info "        published (the gitian step verifies that it is)."
}

# ---------------------------------------------------------------------------
# Phase 2: release-notes check
# ---------------------------------------------------------------------------

# A version number inside a URL is not a discussion of that version: every set
# of notes links to ".../releases/tag/v<version>/" and to the issue tracker, so
# counting those as "mentions" hands a free pass to a file that says nothing.
# Strip link targets before any content check; keep the prose.
notes_prose() {
    sed -E \
        -e 's#<[a-zA-Z][a-zA-Z0-9+.-]*://[^>]*>##g' \
        -e 's#\]\([^)]*\)#]#g' \
        -e 's#\[[^]]*\]:[[:space:]]*[^[:space:]]*##g' \
        -e 's#[a-zA-Z][a-zA-Z0-9+.-]*://[^[:space:]<>)"]*##g' \
        -- "$1"
}

phase_check_notes() {
    head1 "Phase 2: release notes for $VERSION"

    local rel="doc/release-notes/release-notes-$VERSION.md"
    local file="$ROOT/$rel"
    local errors=0

    # Pre-releases are usually documented in the notes for their target
    # version rather than in a file of their own.
    if [[ ! -f $file && -n $V_SUFFIX && -f $ROOT/doc/release-notes/release-notes-$VERSION_CORE.md ]]; then
        rel="doc/release-notes/release-notes-$VERSION_CORE.md"
        file="$ROOT/$rel"
        info "  no notes for the pre-release itself; checking $rel"
    fi

    if [[ ! -f $file ]]; then
        fail "$rel does not exist"
        info ""
        info "  Create it, using an existing release's notes for structure:"
        info "    ls $ROOT/doc/release-notes/"
        return 1
    fi
    ok "$rel exists"

    # --- size ---
    local bytes lines
    bytes=$(wc -c <"$file" | tr -d ' ')
    lines=$(grep -c '[^[:space:]]' "$file" || true)
    if (( bytes < NOTES_MIN_BYTES )); then
        fail "too short: $bytes bytes (minimum $NOTES_MIN_BYTES) - looks like a stub"
        errors=$((errors + 1))
    elif (( lines < NOTES_MIN_LINES )); then
        fail "too thin: $lines non-blank lines (minimum $NOTES_MIN_LINES) - looks like a stub"
        errors=$((errors + 1))
    else
        ok "length plausible: $bytes bytes, $lines non-blank lines"
    fi

    # Everything below reads the prose, not the raw file: see notes_prose.
    # sed does not add or remove lines, so grep -n line numbers stay true.
    local prose
    prose=$(notes_prose "$file")

    # --- placeholder text ---
    #
    # Patterns stay case-SENSITIVE where a lowercase match would be ordinary
    # prose ("earlier drafts of ParseUapOutputScript"), and are case-tolerant
    # where the word can only be a marker. The draft markers are the subtle
    # case: "(DRAFT)" was caught but the equally common "(Draft)", "**Draft**"
    # and "# Draft release notes" sailed straight through. They are caught by
    # shape - bracketed, bolded, a whole line, or a heading - so a sentence
    # about drafting a change is still fine.
    local -a placeholders=(
        '\bTBD\b'
        '\bTODO\b'
        '\bFIXME\b'
        '\bXXX+\b'
        '\bDRAFT\b'
        '\([Dd]raft\)'
        '\[[Dd]raft\]'
        '\{[Dd]raft\}'
        '\*\*[Dd]raft\*\*'
        '_[Dd]raft_'
        '^#{1,6}[[:space:]].*\b[Dd]raft\b'
        '^[[:space:]]*[Dd]raft[[:space:]]*$'
        '\b[Dd]raft [Rr]elease [Nn]otes\b'
        '\b[Pp]reliminary [Dd]raft\b'
        '\b[Tt]his is a [Dd]raft\b'
        '\b[Dd]raft[[:space:]]*[-:][[:space:]]*do not\b'
        '[Ll]orem ipsum'
        '[Pp]laceholder'
        'PLACEHOLDER'
        '<insert[^>]*>'
        '\[insert[^]]*\]'
        '\[fill[^]]*\]'
        '\[[Yy]our [^]]*\]'
        '\bX\.Y\.Z\b'
        '\bN\.N\.N\b'
        '\$\{?VERSION\}?'
        '@@[A-Z_]+@@'
        '\[TBD\]'
        'COPY ME'
        'CHANGEME'
    )
    local pat all_hits=""
    for pat in "${placeholders[@]}"; do
        all_hits+=$(grep -nE "$pat" <<<"$prose" || true)$'\n'
    done
    all_hits=$(printf '%s' "$all_hits" | grep . | sort -t: -k1,1n -u || true)
    if [[ -n $all_hits ]]; then
        fail "placeholder text still present:"
        printf '%s\n' "$all_hits" | sed 's/^/         /'
        errors=$((errors + 1))
    else
        ok "no template placeholders found"
    fi

    # --- the target version must actually be discussed ---
    #
    # Counted in the prose only. The old count ran over the raw file, where the
    # boilerplate "<https://github.com/chromatic/whippet-experimental-blockchain/releases/tag/v1.3.0/>"
    # header alone could supply mentions for a file whose body never names the
    # release at all.
    local mentions ver_re=${VERSION_CORE//./\\.}
    mentions=$(grep -oE "(^|[^0-9.])$ver_re([^0-9.]|$)" <<<"$prose" | grep -c . || true)
    if (( mentions < NOTES_MIN_VERSION_MENTIONS )); then
        fail "notes mention $VERSION_CORE only $mentions time(s) outside of URLs (want >= $NOTES_MIN_VERSION_MENTIONS)"
        errors=$((errors + 1))
    else
        ok "notes mention $VERSION_CORE $mentions time(s) in prose"
    fi

    # --- copy-paste detection: the target must dominate any other version ---
    #
    # A previous release's notes copied to a new filename and left unedited is
    # the classic failure. Such a file talks about the OLD version throughout.
    local top_ver top_count
    read -r top_count top_ver < <(
        grep -oE '\b[0-9]+\.[0-9]+\.[0-9]+\b' <<<"$prose" \
            | sort | uniq -c | sort -rn | head -1
    ) || true
    if [[ -n ${top_ver:-} && $top_ver != "$VERSION_CORE" ]]; then
        fail "notes talk about $top_ver more than $VERSION_CORE ($top_count vs $mentions) - copy-pasted from a previous release?"
        errors=$((errors + 1))
    elif [[ -n ${top_ver:-} ]]; then
        ok "$VERSION_CORE is the most-referenced version in the notes"
    fi

    # --- the headline version must be the target ---
    local declared
    declared=$(head -5 <<<"$prose" | grep -oE '\b[0-9]+\.[0-9]+\.[0-9]+\b' | head -1 || true)
    if [[ -z $declared ]]; then
        fail "no version number in the first 5 lines outside a URL; the notes should open with 'Whippet Core version $VERSION_CORE ...'"
        errors=$((errors + 1))
    elif [[ $declared != "$VERSION_CORE" ]]; then
        fail "notes open by announcing version $declared, not $VERSION_CORE"
        errors=$((errors + 1))
    else
        ok "notes announce version $VERSION_CORE in the opening lines"
    fi

    # --- consistency with configure.ac (informational unless bumping too) ---
    local cur
    cur=$(current_version_from_configure_ac)
    if [[ $cur != "$VERSION_CORE" ]]; then
        warn "configure.ac says $cur, notes are for $VERSION_CORE (run '$SCRIPT_NAME bump $VERSION')"
    else
        ok "configure.ac version matches the notes"
    fi

    info ""
    if (( errors )); then
        fail "release notes check FAILED with $errors problem(s)"
        return 1
    fi
    ok "release notes check PASSED"
    return 0
}

# ---------------------------------------------------------------------------
# Phase 2b: commit and tag  (subcommand "all" only)
#
# Why this exists at all:
#
#   "all" used to run check_tag_absent (refuse if v<version> exists) and then
#   phase_gitian, whose tag check refuses if v<version> does NOT exist. Those
#   two requirements cannot both hold in one process, so "all" could never
#   succeed - it either stopped at the guard or stopped at the gitian check.
#
# The contradiction is resolved in the only direction that is actually correct
# for the release order: the tag must not exist when the bump starts (or the
# bump would be rewriting files after the thing that pins them), and it must
# exist by the time gitian runs. So "all" creates it in between, from the very
# commit the bump produced. That is also precisely the ordering whose absence
# caused the 1.1.0 bug: tag first, bump later, build the wrong sources.
#
# This is the only place the script writes to git history, it is reached only
# via "all", and it needs --yes.
# ---------------------------------------------------------------------------

phase_tag() {
    head1 "Phase 2b: commit and tag v$VERSION"

    in_git_repo || die "$ROOT is not a git repository; cannot commit or tag"

    local -a files=()
    if [[ $DRY_RUN == true ]]; then
        # Nothing was written, so take the list from what the bump reported.
        if (( ${#CHANGED[@]} )); then
            mapfile -t files < <(printf '%s\n' "${CHANGED[@]}" \
                | sed -n 's/^\([^: ]*\):[0-9].*/\1/p' | sort -u)
        fi
    elif (( ${#WRITTEN_FILES[@]} )); then
        mapfile -t files < <(printf '%s\n' "${WRITTEN_FILES[@]}" | sort -u)
    fi

    local tag_exists=false
    git -C "$ROOT" rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null && tag_exists=true

    if [[ $tag_exists == true ]]; then
        # check_tag_absent already ran; getting here means --allow-existing-tag.
        # Moving a published tag is never done silently.
        warn "tag v$VERSION already exists; leaving it exactly where it is"
        info "  If it predates this bump, delete it and re-run:"
        info "    git -C $ROOT tag -d v$VERSION"
        if (( ${#files[@]} )); then
            info "  (the bump wrote ${#files[@]} file(s) that this tag does NOT contain)"
        fi
        return 0
    fi

    local -a commit_cmd tag_cmd
    commit_cmd=(git -C "$ROOT" commit -m "version: bump to $VERSION")
    if [[ $SIGN_TAG == true ]]; then
        tag_cmd=(git -C "$ROOT" tag -s "v$VERSION" -m "Whippet Core $VERSION")
    else
        tag_cmd=(git -C "$ROOT" tag -a "v$VERSION" -m "Whippet Core $VERSION")
    fi

    info "  will stage:  ${files[*]:-(nothing - bump was a no-op)}"
    info "  will commit: $(printf '%q ' "${commit_cmd[@]}")"
    info "  will tag:    $(printf '%q ' "${tag_cmd[@]}")"

    if [[ $DRY_RUN == true ]]; then
        # Tell phase_gitian that the tag it is about to look for is one this
        # very run would have created a moment ago; otherwise every
        # "all --dry-run" would report a self-inflicted failure.
        TAG_PENDING=true
        info ""
        info "  dry-run: no commit and no tag were created"
        return 0
    fi

    if (( ${#files[@]} )); then
        git -C "$ROOT" add -- "${files[@]}" \
            || die "failed to stage the bumped files"
        if git -C "$ROOT" diff --cached --quiet; then
            info "  nothing staged after all; skipping the commit"
        else
            "${commit_cmd[@]}" || die "failed to commit the version bump"
            ok "committed $(git -C "$ROOT" rev-parse --short HEAD)"
        fi
    else
        info "  bump changed nothing; tagging the current HEAD"
    fi

    "${tag_cmd[@]}" || die "failed to create tag v$VERSION (for -s, git needs user.signingkey; or pass --no-sign-tag)"
    ok "created tag v$VERSION at $(git -C "$ROOT" rev-parse --short "v$VERSION^{commit}")"
}

# ---------------------------------------------------------------------------
# Phase 3: gitian build
#
# This wraps contrib/gitian-build.sh, which is the real tooling: it sets up the
# gitian environment, downloads the descriptors for the requested commit from
# GitHub, runs gbuild/gsign, and collects binaries into gitian-output/.
#
# It replaces the root-level run-gitian-build.sh, whose core insight is kept
# (gitian-build.sh fetches descriptors from raw.githubusercontent.com at the
# tag, so the tag must exist on the remote before a build can start, and the
# descriptors AT THAT TAG must already pin the right commit) but whose
# implementation swallowed push failures with "|| true", checked no
# prerequisites, and asked for no confirmation before a multi-hour build.
# ---------------------------------------------------------------------------

normalize_platforms() {
    local in=${PLATFORMS,,} out=""
    [[ $in == *linux* || $in == *l* ]] && out+="l"
    [[ $in == *win*   || $in == *w* ]] && out+="w"
    [[ $in == *osx*   || $in == *x* || $in == *mac* ]] && out+="x"
    # "linux" contains an x; re-derive from words when words were used
    if [[ $in == *linux* || $in == *win* || $in == *osx* || $in == *mac* ]]; then
        out=""
        [[ $in == *linux* ]] && out+="l"
        [[ $in == *win*   ]] && out+="w"
        [[ $in == *osx* || $in == *mac* ]] && out+="x"
    fi
    [[ -n $out ]] || die "--platforms '$PLATFORMS': nothing selected (use l, w, x or linux,win,osx)"
    PLATFORMS_CODE=$out
    PLATFORM_NAMES=()
    [[ $out == *l* ]] && PLATFORM_NAMES+=("linux")
    [[ $out == *w* ]] && PLATFORM_NAMES+=("win")
    [[ $out == *x* ]] && PLATFORM_NAMES+=("osx")
}

require_tool() {
    command -v "$1" >/dev/null 2>&1 || MISSING+=("$1 ($2)")
}

phase_gitian() {
    head1 "Phase 3: gitian build for v$VERSION"

    normalize_platforms
    info "  platforms: ${PLATFORM_NAMES[*]}  (-o $PLATFORMS_CODE)"
    info "  remote:    $REMOTE"
    info ""

    # ---- prerequisites, all checked BEFORE anything is launched ----
    head1 "Prerequisite checks"
    local problems=0
    declare -a MISSING=()

    if [[ -x $ROOT/contrib/gitian-build.sh ]]; then
        ok "contrib/gitian-build.sh present and executable"
    elif [[ -f $ROOT/contrib/gitian-build.sh ]]; then
        fail "contrib/gitian-build.sh is not executable (chmod +x it)"
        problems=$((problems + 1))
    else
        fail "contrib/gitian-build.sh is missing"
        problems=$((problems + 1))
    fi

    local p
    for p in "${PLATFORM_NAMES[@]}"; do
        if [[ -f $ROOT/gitian-descriptors/gitian-$p.yml ]]; then
            ok "descriptor gitian-descriptors/gitian-$p.yml present"
        else
            fail "descriptor gitian-descriptors/gitian-$p.yml is missing"
            problems=$((problems + 1))
        fi
    done

    if [[ -d $ROOT/gitian-builder ]]; then
        ok "gitian-builder/ present"
    elif [[ $GITIAN_SETUP == true ]]; then
        ok "gitian-builder/ absent, but --setup was requested"
    else
        fail "gitian-builder/ is missing (run once with --setup, or clone https://github.com/devrandom/gitian-builder)"
        problems=$((problems + 1))
    fi

    require_tool git "version control"
    require_tool curl "descriptor download"
    require_tool ruby "gbuild is a ruby script"
    case "$VIRT" in
        --docker) require_tool docker "requested by --docker" ;;
        --lxc)    require_tool lxc-execute "requested by --lxc" ;;
        *)        require_tool kvm "default virtualization; pass --docker or --lxc to use another" ;;
    esac
    if (( ${#MISSING[@]} )); then
        local m
        for m in "${MISSING[@]}"; do
            fail "required tool not found: $m"
        done
        problems=$((problems + ${#MISSING[@]}))
    else
        ok "required tools present"
    fi

    if [[ $NO_SIGN == true ]]; then
        ok "signing disabled (--no-sign)"
    elif [[ -n $SIGNER ]]; then
        ok "gitian signer: $SIGNER"
        if command -v gpg >/dev/null 2>&1; then
            gpg --list-secret-keys "$SIGNER" >/dev/null 2>&1 \
                || warn "gpg has no secret key matching '$SIGNER'; gsign will fail"
        else
            warn "gpg not installed; signing will fail"
        fi
    else
        fail "no signer configured: pass --signer NAME, set \$SIGNER, or pass --no-sign"
        problems=$((problems + 1))
    fi

    # ---- tag and descriptor consistency ----
    head1 "Tag checks"
    if ! in_git_repo; then
        fail "$ROOT is not a git repository; cannot verify the release tag"
        problems=$((problems + 1))
    else
        if git -C "$ROOT" rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null; then
            ok "tag v$VERSION exists locally"

            if [[ $SKIP_DESCRIPTOR_CHECK == true ]]; then
                warn "skipping descriptor pin verification (--skip-descriptor-check)"
            else
                # The 1.1.0 lesson: gitian builds whatever the descriptors AT
                # THE TAG say, so a tag created before the descriptor bump
                # produces binaries built from the previous release's sources.
                for p in "${PLATFORM_NAMES[@]}"; do
                    local pinned
                    pinned=$(git -C "$ROOT" show "v$VERSION:gitian-descriptors/gitian-$p.yml" 2>/dev/null \
                        | sed -n 's/^ *"commit": *"\(.*\)"$/\1/p' | head -1 || true)
                    if [[ -z $pinned ]]; then
                        fail "tag v$VERSION has no readable commit pin in gitian-descriptors/gitian-$p.yml"
                        problems=$((problems + 1))
                    elif [[ $pinned != "v$VERSION" ]]; then
                        fail "tag v$VERSION pins gitian-$p.yml to '$pinned', not 'v$VERSION' - the build would produce mislabelled binaries. Re-run 'bump', commit, and move the tag."
                        problems=$((problems + 1))
                    else
                        ok "tagged gitian-$p.yml pins v$VERSION"
                    fi
                done
            fi
        elif [[ $TAG_PENDING == true ]]; then
            # "all --dry-run": phase 2b would have created this tag seconds ago.
            # Reporting its absence would be reporting a failure this run
            # invented for itself.
            ok "tag v$VERSION would exist by now (created in phase 2b of this run)"
            info "        its descriptors are the ones phase 1 just rewrote, so the"
            info "        pin check cannot run in a dry run and is deferred."
        else
            fail "tag v$VERSION does not exist locally; create it first (git tag -s v$VERSION)"
            problems=$((problems + 1))
        fi

        if git -C "$ROOT" remote get-url "$REMOTE" >/dev/null 2>&1; then
            ok "remote '$REMOTE' configured"

            # Without --push-tag we are not going to publish the tag, so the
            # tag has to be published already: gitian-build.sh downloads the
            # descriptors from the remote at this tag and fails obscurely if
            # they are not there.
            if [[ $PUSH_TAG == false ]]; then
                local remote_tag
                if remote_tag=$(git -C "$ROOT" ls-remote --tags "$REMOTE" "refs/tags/v$VERSION" 2>/dev/null); then
                    if [[ -n $remote_tag ]]; then
                        ok "tag v$VERSION is already on '$REMOTE' (no push needed)"
                    else
                        fail "tag v$VERSION is not on remote '$REMOTE' and pushing is not authorized; pass --push-tag to publish it"
                        problems=$((problems + 1))
                    fi
                else
                    warn "could not query '$REMOTE' for v$VERSION; the build needs that tag to be published"
                fi
            else
                ok "pushing v$VERSION to '$REMOTE' is authorized (--push-tag)"
            fi
        else
            fail "no git remote named '$REMOTE' (use --remote)"
            problems=$((problems + 1))
        fi
    fi

    if (( problems )); then
        info ""
        if [[ $DRY_RUN == true ]]; then
            # Do not let a dry run claim success for a run that would abort.
            warn "$problems prerequisite problem(s) - a real run would stop here"
            DRY_RUN_WOULD_FAIL=true
        else
            die "$problems prerequisite problem(s); nothing was launched"
        fi
    fi

    # ---- assemble the command ----
    local repo_url
    repo_url=$(git -C "$ROOT" remote get-url "$REMOTE" 2>/dev/null || echo "<remote $REMOTE not configured>")
    # gitian-build.sh's descriptor downloader needs an https github.com URL.
    if [[ $repo_url == git@github.com:* ]]; then
        repo_url="https://github.com/${repo_url#git@github.com:}"
    fi
    repo_url=${repo_url%.git}
    repo_url=${repo_url%/}

    local -a cmd=("$ROOT/contrib/gitian-build.sh" -u "$repo_url" -o "$PLATFORMS_CODE")
    [[ $GITIAN_SETUP == true ]] && cmd+=(--setup)
    [[ -n $VIRT ]] && cmd+=("$VIRT")
    [[ -n $GITIAN_JOBS ]] && cmd+=(-j "$GITIAN_JOBS")
    [[ -n $GITIAN_MEM ]] && cmd+=(-m "$GITIAN_MEM")
    [[ $NO_SIGN == false && -n $SIGNER ]] && cmd+=(-s "$SIGNER")
    cmd+=(--build "$VERSION")

    head1 "Planned actions"
    if [[ $PUSH_TAG == true ]]; then
        info "  1. git -C $ROOT push $REMOTE v$VERSION      ${C_YEL}(authorized by --push-tag)${C_OFF}"
        info "     (contrib/gitian-build.sh fetches descriptors from"
        info "      raw.githubusercontent.com at this tag, so it must be pushed first)"
    else
        info "  1. (no push: --push-tag was not given; the tag must already be published)"
    fi
    info "  2. cd $ROOT && ${cmd[*]}"
    info ""
    info "  Output will land in $ROOT/gitian-output/whippet-binaries/$VERSION/"
    info "  Expect 2-4 hours per platform. This is hard to interrupt cleanly."

    if [[ $DRY_RUN == true ]]; then
        info ""
        warn "dry-run: nothing launched"
        return 0
    fi
    if [[ $ASSUME_YES != true ]]; then
        info ""
        die "refusing to launch a multi-hour build without --yes"
    fi

    head1 "Launching"
    if [[ $PUSH_TAG == true ]]; then
        info "  pushing v$VERSION to $REMOTE (authorized by --push-tag) ..."
        # Deliberately NOT "|| true": if the tag is not on the remote, the
        # descriptor download later fails in a far more confusing way.
        git -C "$ROOT" push "$REMOTE" "v$VERSION" \
            || die "failed to push v$VERSION to $REMOTE (if it is already pushed and identical, re-run without --push-tag)"
    fi

    info "  running: ${cmd[*]}"
    ( cd "$ROOT" && "${cmd[@]}" )
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

main() {
    parse_args "$@"
    validate_version
    resolve_root

    info "$C_BLD Whippet Core release tool$C_OFF"
    info "  root:       $ROOT"
    info "  version:    $VERSION"
    info "  subcommand: $SUBCOMMAND"
    [[ $DRY_RUN == true ]] && info "  mode:       ${C_YEL}DRY RUN - no files will be written, nothing launched${C_OFF}"

    # "all" ends in a commit, a tag and a multi-hour build. Refuse before the
    # first byte is written rather than after the bump, so a missing --yes
    # never leaves a rewritten tree behind.
    if [[ $SUBCOMMAND == all && $DRY_RUN == false && $ASSUME_YES != true ]]; then
        info ""
        die "'all' bumps, commits, tags v$VERSION and then runs a multi-hour build; \
re-run with --yes (add --push-tag to also publish the tag), or with --dry-run to preview"
    fi

    local rc=0
    case "$SUBCOMMAND" in
        bump)
            head1 "Guards"
            check_clean_tree
            check_tag_absent
            phase_bump
            ;;
        check-notes)
            phase_check_notes || rc=1
            ;;
        gitian)
            phase_gitian || rc=1
            ;;
        all)
            head1 "Guards"
            check_clean_tree
            # The tag must NOT exist yet: "all" creates it below, from the
            # commit the bump produces, and hands the existing tag to gitian.
            check_tag_absent
            phase_bump
            phase_check_notes || rc=1
            if (( rc )); then
                info ""
                die "release notes are not ready; fix them before building (bump changes above are kept, uncommitted and untagged)"
            fi
            phase_tag
            phase_gitian || rc=1
            ;;
        *)  # parse_args already validated this; belt and braces.
            die "unhandled subcommand: $SUBCOMMAND" ;;
    esac

    if (( ${#WARNINGS[@]} )); then
        head1 "Warnings (${#WARNINGS[@]})"
        local w
        for w in "${WARNINGS[@]}"; do
            info "  - $w"
        done
    fi

    # A dry run that hit something fatal must not exit 0: the whole point of
    # previewing is to learn whether the real run would succeed.
    if [[ $DRY_RUN == true && $DRY_RUN_WOULD_FAIL == true && $rc -eq 0 ]]; then
        info ""
        fail "dry-run: the real run would have FAILED (see the warnings above)"
        rc=1
    fi

    return $rc
}

main "$@"
