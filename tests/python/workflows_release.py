"""Release, publish and tag policy."""

from __future__ import annotations

import re
import shlex

from .workflows_support import (
    WORKFLOWS,
    logical_command_lines,
    missing_needles,
    named_step,
    steps,
)

PUBLISH_GUARD = (
    "always() && needs.required.result == 'success' && "
    "github.repository == 'LLM-4-People/millivolt' && "
    "((github.event_name == 'push' && "
    "(github.ref == 'refs/heads/main' || startsWith(github.ref, 'refs/tags/v'))) || "
    "(github.event_name == 'pull_request' && github.event.action == 'closed' && "
    "github.event.pull_request.merged == true && github.base_ref == 'main' && "
    "github.event.pull_request.head.repo.full_name == github.repository && "
    "needs.required.outputs.merged_docs_only != 'true'))"
)
PUBLISH_TESTING_GUARD = (
    "github.event_name == 'push' && github.repository == 'LLM-4-People/millivolt' && "
    "github.ref == 'refs/heads/testing'"
)
PUBLISH_GUARD_NEEDLES = (
    "always()",
    "needs.required.result == 'success'",
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/main'",
    "startsWith(github.ref, 'refs/tags/v')",
    "github.event_name == 'pull_request'",
    "github.event.action == 'closed'",
    "github.event.pull_request.merged == true",
    "github.base_ref == 'main'",
    "github.event.pull_request.head.repo.full_name == github.repository",
    "needs.required.outputs.merged_docs_only != 'true'",
)
PUBLISH_TESTING_GUARD_NEEDLES = (
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/testing'",
)


def publish_guard_errors(publish_if: str, publish_testing_if: str) -> list[str]:
    """Reject any publication condition beyond the exact normalized policy.

    Positive needles alone let an extra `||` ref ride along. The exact string
    makes every event, repository and ref term part of the contract, so a new
    disjunct fails even when all required fragments remain present.
    """
    errors = []
    if publish_if != PUBLISH_GUARD:
        errors.append("check.yml: publish guard must be exactly " + PUBLISH_GUARD)
    if publish_testing_if != PUBLISH_TESTING_GUARD:
        errors.append("check.yml: publish-testing guard must be exactly " + PUBLISH_TESTING_GUARD)
    return errors


def publish_concurrency_errors(concurrency) -> list[str]:
    """The release group owns check-then-create atomicity for the version tag.

    A cancelling group (or a missing flag) could start a second publish while
    the first is between its tag guard and its release create, so both could
    pass the check and publish. The release job must serialize without
    cancellation.
    """
    if not isinstance(concurrency, dict):
        return ["check.yml: publish must declare its release concurrency group"]
    errors = []
    if concurrency.get("group") != "release":
        errors.append("check.yml: publish concurrency group must be release")
    if concurrency.get("cancel-in-progress") is not False:
        errors.append("check.yml: publish concurrency must set cancel-in-progress: false")
    return errors


MAIN_REF_NAMES = {"main", "refs/heads/main"}
# `git` may carry options before the subcommand, including a value-taking
# `-c key=value` or `-C path`. Stop at a command separator so a later `push`
# in another command cannot make this one look like a push.
GIT_PUSH = re.compile(r"\bgit\b[^;|&]*?\bpush\b")
MAIN_BROAD_PUSH_FLAGS = {"--all", "--branches", "--mirror"}


def main_push_errors(text: str) -> list[str]:
    """Reject every non-comment git push whose destination is main.

    Covers bare and fully qualified refs, `<src>:main` and `:main` deletion
    refspecs, `main:` with an empty destination, `--delete main`, broad
    `--all`/`--branches`/`--mirror` pushes, `git -c ... push`, and commands
    split over continuation lines.
    """
    errors = []
    for line in logical_command_lines(text):
        # A line can chain commands. Check every segment so a safe push before
        # `;` or `&&` cannot hide a main push after it.
        line_has_error = False
        for segment in re.split(r"\s*(?:;|&&|\|\||\|)\s*", line):
            match = GIT_PUSH.search(segment)
            if not match:
                continue
            try:
                arguments = shlex.split(segment[match.end():])
            except ValueError:
                arguments = segment[match.end():].split()
            if any(argument in MAIN_BROAD_PUSH_FLAGS for argument in arguments):
                errors.append("check.yml: a git push line can target main: " + line.strip())
                line_has_error = True
                break
            for argument in arguments:
                if argument.startswith("-"):
                    continue
                refspec = argument.lstrip("+")
                if ":" in refspec:
                    source, destination = refspec.split(":", 1)
                    # `:main` deletes main; `main:` pushes main to an empty
                    # destination, which is still a main refspec.
                    if (destination in MAIN_REF_NAMES or
                            (not destination and source in MAIN_REF_NAMES)):
                        errors.append("check.yml: a git push line targets main: " + line.strip())
                        line_has_error = True
                        break
                elif refspec in MAIN_REF_NAMES:
                    errors.append("check.yml: a git push line targets main: " + line.strip())
                    line_has_error = True
                    break
            if line_has_error:
                break
    return errors


def check_release(checks: dict, errors: list[str]) -> None:
    jobs = checks.get("jobs") or {}
    publish = jobs.get("publish") or {}
    if publish.get("needs") not in ("required", ["required"]):
        errors.append("check.yml: publish must depend on the required gate")
    publish_if = " ".join(str(publish.get("if", "")).split())
    publish_missing = missing_needles(publish_if, PUBLISH_GUARD_NEEDLES)
    for needle in publish_missing:
        errors.append(f"check.yml: publish guard is missing {needle}")
    publish_testing = jobs.get("publish-testing") or {}
    if publish_testing.get("needs") != ["unit", "browser", "container"]:
        errors.append("check.yml: publish-testing must still wait for unit, browser "
                      "and container")
    publish_testing_if = " ".join(str(publish_testing.get("if", "")).split())
    publish_testing_missing = missing_needles(publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES)
    for needle in publish_testing_missing:
        errors.append(f"check.yml: publish-testing guard is missing {needle}")
    errors.extend(publish_guard_errors(publish_if, publish_testing_if))
    errors.extend(publish_concurrency_errors(publish.get("concurrency")))
    # Mutation proofs: a cancelling group, a renamed group and a missing flag
    # must all fail, so the atomicity owner cannot silently drift.
    for label, concurrency in (
        ("a cancelling release group", {"group": "release", "cancel-in-progress": True}),
        ("a renamed release group", {"group": "publish", "cancel-in-progress": False}),
        ("a missing cancel flag", {"group": "release"}),
        ("a missing concurrency block", None),
    ):
        if not publish_concurrency_errors(concurrency):
            errors.append(f"workflows self-check: {label} must fail")
    # Mutation proofs: dropping any one event, repository or ref condition from
    # either publication guard must be caught, not masked by the others, and an
    # extra `||` ref must fail the exact normalized contract.
    for guard, needles, label in (
        (publish_if, PUBLISH_GUARD_NEEDLES, "publish"),
        (publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES, "publish-testing"),
    ):
        for needle in needles:
            if not missing_needles(guard.replace(needle, ""), needles):
                errors.append(f"workflows self-check: removing the {label} guard {needle} must fail")
    forbidden_publish = publish_if + " || github.ref == 'refs/heads/testing'"
    if not publish_guard_errors(forbidden_publish, publish_testing_if):
        errors.append("workflows self-check: adding a forbidden ref to the publish guard must fail")
    forbidden_testing = publish_testing_if + " || github.ref == 'refs/heads/main'"
    if not publish_guard_errors(publish_if, forbidden_testing):
        errors.append("workflows self-check: adding a forbidden ref to the publish-testing guard must fail")
    # A main push deliberately skips checks and container. Without always(),
    # GitHub's implicit success gate also skips publish even though required
    # passes, so the release never gets tagged.
    without_always = publish_if.replace("always() && ", "", 1)
    if not publish_guard_errors(without_always, publish_testing_if):
        errors.append("workflows self-check: dropping always() from publish must fail")
    without_required = publish_if.replace("needs.required.result == 'success' && ", "", 1)
    if not publish_guard_errors(without_required, publish_testing_if):
        errors.append("workflows self-check: dropping the required success check must fail")

    publish_checkout = next((step for step in steps(publish)
                             if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    publish_ref = " ".join(str((publish_checkout.get("with") or {}).get("ref", "")).split())
    if "github.event.pull_request.merge_commit_sha" not in publish_ref:
        errors.append("check.yml: merged-PR publish checkout must use the event merge commit")
    if "github.ref" not in publish_ref:
        errors.append("check.yml: branch and tag publish checkout must keep the pushed ref")
    verify_target = next((step for step in steps(publish)
                          if step.get("name") == "Verify release target"), {})
    verify_run = str(verify_target.get("run", ""))
    if "git rev-parse HEAD" not in verify_run or '!= "$EXPECTED"' not in verify_run:
        errors.append("check.yml: merged-PR publish must verify the checked-out merge commit")

    image_index = named_step(publish, "Publish verified multi-platform image")
    tag_push_index = named_step(publish, "Push verified tag")
    release_index = named_step(publish, "Create the GitHub release")
    if min(image_index, tag_push_index, release_index) < 0:
        errors.append("check.yml: publish image, tag push and release steps must all exist")
    elif not (image_index < tag_push_index < release_index):
        errors.append("check.yml: image must be published before the tag and release")
    image = steps(publish)[image_index] if image_index >= 0 else {}
    if image.get("with", {}).get("sbom") is not True:
        errors.append("check.yml: release image must include an SBOM")
    if image.get("with", {}).get("provenance") != "mode=max":
        errors.append("check.yml: release image must include max provenance")
    attest_index = named_step(publish, "Attest the published image")
    if attest_index < 0:
        errors.append("check.yml: release image must carry a GitHub artifact attestation")
    elif not (image_index < attest_index < tag_push_index):
        errors.append("check.yml: the image attestation must finish before the tag becomes public")
    else:
        attest = steps(publish)[attest_index]
        if not str(attest.get("uses", "")).startswith("actions/attest@"):
            errors.append("check.yml: the image attestation must use actions/attest")
        attest_with = attest.get("with") or {}
        if attest_with.get("subject-digest") != "${{ steps.image.outputs.digest }}":
            errors.append("check.yml: the attestation must cover the published image digest")
        if attest_with.get("push-to-registry") is not True:
            errors.append("check.yml: the attestation must be pushed to the registry")
    cut = next((step for step in steps(publish) if step.get("name") == "Cut the next version"), {})
    cut_run = str(cut.get("run", ""))
    if "git push" in cut_run:
        errors.append("check.yml: the cut step must not push before the image exists")
    if "git commit" in cut_run:
        errors.append("check.yml: the release workflow must not create version commits")
    if "GH_TOKEN" not in (cut.get("env") or {}):
        errors.append("check.yml: the cut step must read the GitHub release with GH_TOKEN")
    release_guard = (
        'tagged_revision="$(git rev-list -n1 "v$file_version")"',
        'gh release view "v$file_version"',
        "GitHub release is missing",
        "does not move the tag or republish images",
        "exit 1",
    )
    for needle in missing_needles(cut_run, release_guard):
        errors.append(f"check.yml: cut step is missing the missing-release guard {needle}")
    for needle in release_guard:
        if not missing_needles(cut_run.replace(needle, ""), release_guard):
            errors.append(f"workflows self-check: removing the missing-release guard {needle} must fail")
    tag_push = steps(publish)[tag_push_index] if tag_push_index >= 0 else {}
    tag_push_run = str(tag_push.get("run", ""))
    if '"v$RELEASE_VERSION"' not in tag_push_run:
        errors.append("check.yml: the release workflow must push the verified tag")
    if "skip != 'true'" not in str(tag_push.get("if", "")):
        errors.append("check.yml: the tag push must honor the no-release guard")
    release_step = steps(publish)[release_index] if release_index >= 0 else {}
    release_run = str(release_step.get("run", ""))
    for needle in (
        "## Highlights",
        "## Compatibility",
        "## Upgrade",
        "docker pull ghcr.io/llm-4-people/millivolt:${RELEASE_VERSION}",
        "<summary>Other changes</summary>",
        "gh attestation verify oci://ghcr.io/llm-4-people/millivolt:${RELEASE_VERSION}",
    ):
        if needle not in release_run:
            errors.append(f"check.yml: release notes are missing {needle}")
    summary_step = next((step for step in steps(publish)
                         if step.get("name") == "Summarize the release"), {})
    summary_run = str(summary_step.get("run", ""))
    if not summary_step:
        errors.append("check.yml: publish must summarize the release in the run summary")
    else:
        if "always()" not in str(summary_step.get("if", "")):
            errors.append("check.yml: the release summary must run for both success and skip")
        for needle in ("No release cut", "releases/tag/v${RELEASE_VERSION}", "GITHUB_STEP_SUMMARY",
                       "job.status", "no release receipt is claimed"):
            if needle not in summary_run:
                errors.append(f"check.yml: release summary is missing {needle}")
        if "VERSION is already tagged at HEAD" in summary_run:
            errors.append("check.yml: the skip summary must not claim the tag is at HEAD")
    meta = next((step for step in steps(publish)
                 if step.get("name") == "Define image tags and labels"), {})
    meta_with = meta.get("with") or {}
    if "org.opencontainers.image.description" not in str(meta_with.get("annotations", "")):
        errors.append("check.yml: release image must carry a package description annotation")
    if (meta.get("env") or {}).get("DOCKER_METADATA_ANNOTATIONS_LEVELS") != "index,manifest":
        errors.append("check.yml: package annotations must target the index and manifest")
    publish_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    errors.extend(main_push_errors(publish_text))
    for label, line in (
        ("a bare main refspec", "git push origin main"),
        ("a fully qualified main refspec", "git push origin refs/heads/main"),
        ("a main refspec destination", "git push origin HEAD:refs/heads/main"),
        ("a same-name main destination", "git push origin main:main"),
        ("a short main destination", "git push origin HEAD:main"),
        ("a main deletion refspec", "git push origin :main"),
        ("a qualified main deletion refspec", "git push origin :refs/heads/main"),
        ("an empty main destination", "git push origin main:"),
        ("an empty qualified main destination", "git push origin refs/heads/main:"),
        ("a git -c main push", "git -c user.name=release push origin main"),
        ("a git -c main destination", "git -c advice.detachedHead=false push origin HEAD:main"),
        ("a git option before push", "git --no-pager push origin main"),
        ("a --delete main branch", "git push --delete origin main"),
        ("a broad --all push", "git push --all origin"),
        ("a continued main push", "git push \\\n  origin main"),
        ("a quoted main destination", "git push origin HEAD:'refs/heads/main'"),
        ("a main push chained after a safe push",
         "git push origin HEAD:testing; git push origin main"),
        ("a main push chained with &&",
         "git push origin HEAD:testing && git -c user.name=release push origin main"),
        ("a continued git -c main push",
         "git -c \\\n  user.name=release push \\\n  origin main"),
    ):
        if not main_push_errors(line):
            errors.append(f"workflows self-check: {label} must fail")
    for label, line in (
        ("a testing refspec push", "git push origin HEAD:testing"),
        ("a source-main testing destination", "git push origin main:testing"),
        ("a qualified testing destination", "git push origin HEAD:refs/heads/testing"),
        ("a git -c testing push", "git -c user.name=release push origin HEAD:testing"),
        ("a --delete feature branch", "git push --delete origin feature"),
        ("a tags-only push", "git push --tags origin"),
        ("a continued testing push", "git push \\\n  origin HEAD:testing"),
    ):
        if main_push_errors(line):
            errors.append(f"workflows self-check: {label} must pass")
    if "ghcr.io/${REPO}" in publish_text or "ghcr.io/LLM-4-People" in publish_text:
        errors.append("check.yml: release image references must use lowercase GHCR paths")
    if "value=main,enable=${{ github.ref == 'refs/heads/main' }}" in publish_text:
        errors.append("check.yml: main image must exclude prereleases")
