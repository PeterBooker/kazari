---
name: docs-writer
description: House style for Kazari's documentation, modelled on the Laravel docs - every page in the same order (lede, the model, tasks in the order of use, troubleshooting, next steps), short paragraphs, a lead-in sentence before every code block, goal-first sentences, a heading for every option, defaults stated, no marketing. Use when writing, rewriting, restructuring or reviewing docs/*.md, site/content/docs/*.md or README.md, including the documentation for a new feature.
---

# Writing Kazari docs

The Laravel docs are easy to read because every page has the same shape and every section moves the same way: what it is for, the smallest example that works, what that example did, then one option at a time. Kazari's docs keep that shape and leave out Laravel's sales talk and padding ([Left out on purpose](#left-out-on-purpose)).

This skill covers how a page reads. The mechanics live elsewhere: mounts, links, alerts and `nav.toml` in `.claude/rules/docs.md`, the files that state each behaviour in the `docs-check` skill, the Hugo check in `CLAUDE.md`. It does not apply to `docs/adr/` or the findings logs in `docs/security.md`, which record decisions as they were made.

## Readers

Know which reader a page is for before writing it:

| Pages | Reader | What they want |
| ----- | ------ | -------------- |
| Guides in `site/content/docs/` | People who publish pages, many of them not developers | A task done, with no internals |
| `install`, `configuration`, `identity`, `ops` | The administrator of an instance | Set-up steps, then each setting and what it changes |
| `mcp`, `cli`, `api` | Someone looking up one item | Each item complete on its own and findable from the contents |

## Page shape

1. **Title**: the subject in a few words.
2. **Lede**, with no heading: one to three short paragraphs on what the subject is, the problem it solves, what this page covers and what the reader needs first (with a link).
3. **The model**, when the tasks depend on one: a first H2 that explains how the thing works and separates terms readers confuse ("An *export* is one run; a *schedule* decides when runs happen").
4. **Tasks in the order the reader meets them**: setting up, everyday use, changing it, running it in production, undoing or removing it.
5. **Troubleshooting**: one H3 per symptom, named as the reader sees it, such as the error text or status code.
6. **Next steps**, on guides and getting-started pages: a bullet list of links, each followed by a phrase saying what is there.

Throughout, put the common case before the options, the default before the alternatives, and the recommended path first, saying so plainly: "Most instances use `acme`; use `files` when another tool manages the certificates."

A skeleton, with a made-up subject:

```markdown
# Scheduled exports

Exports copy every record to object storage on a schedule, so a lost disk costs at most one day of changes. This page sets up a schedule, runs an export by hand and restores from one. It assumes a running server ([installation guide](install.md)).

## How exports work

## Creating a schedule

### Choosing a destination

#### Encrypting exports

## Running an export by hand

## Restoring from an export

## Troubleshooting

### `403 Forbidden` from the bucket

## Next steps
```

## Headings

- Sentence case. Give every option, variant or recipe a reader might look for its own heading, so the contents work as an index.
- H2 for a task or concept, H3 for its variants, H4 for a detail that deserves a name but not a contents entry (the site's "On this page" lists H2 and H3 only). Never skip a level.
- Task headings are gerunds: `Creating a schedule`, `Rotating keys`. Concepts and reference items are nouns or the exact name: `Precedence`, `` `listen` ``, `Exit codes`. Numbered imperatives (`1. Start the server`) belong only in a tutorial where the order is the point, such as the quick start.
- Name a task heading after the goal, not the mechanism: `Keeping more exports`, not `` The `--keep` flag ``.
- No hand-written contents list and no `<a name>` anchors: the site builds "On this page" and GitHub shows an outline. An anchor is the GitHub-style id of its heading, so renaming a heading breaks links to it. Grep `docs/`, `site/` and `README.md` for the old `#anchor` and fix every hit.

## Sections

Each section moves the same way:

1. One or two sentences: what this does and when you need it.
2. A lead-in sentence that ends in a colon and says what the block does.
3. The smallest example that works.
4. One sentence on the result: what changed, what it printed, or where to look next.
5. Variants, most common first, one paragraph each with its own lead-in and example.
6. A warning, if one is needed, directly after the step it guards.

For example:

````markdown
## Running an export by hand

A manual export runs outside the schedule, for example before an upgrade. Start one by naming the schedule:

```sh
app exports run nightly
```

The command waits for the export to finish and prints the object's address in the bucket.

To export only the records changed since the last run, add `--incremental`:

```sh
app exports run nightly --incremental
```
````

A reference item (a command, endpoint, tool or key) moves the same way after its synopsis: what it does in one sentence, then what a caller needs to know (defaults, limits, errors), then an example.

## Sentences

- **One idea per paragraph**, in one to three sentences. A paragraph that needs a chain of semicolons or several parentheses is really several paragraphs, a list or a table.
- **Goal first, then the means**: "To keep thirty exports, set `exports.keep` to `30`." Not: "The `exports.keep` setting can be raised if more exports should be kept."
- **Condition first** when only some readers need the sentence: "If the bucket is in another region, set `region` as well."
- **What, then why**: after a command, say what it did; after a rule, give the reason ("Keep the token out of the YAML file, which must be world-readable").
- **Defaults in words**: "By default, exports run at 02:00 UTC." Readers miss a default that appears only inside a code sample.
- **Verbs carry the obligation**: the imperative for steps, "must" for requirements, "can" for options. Avoid "you may", which reads as both permission and possibility.
- **"You" is the reader and "Kazari" is the software.** No "we", "let's" or "our".
- **Present tense, active voice**: "Kazari removes the file", not "the file will be removed".
- **One name per thing**: define a term at first use, then use that word every time. A synonym reads as a second thing.
- **House spelling**: British English (colour, behaviour, organisation, licence) and no contractions. Names from specifications keep their spelling: authorization server, the `Authorization` header.

## Words to cut

| Instead of | Write |
| ---------- | ----- |
| simply, just, easily, of course, obviously | Nothing. When the step fails, these words blame the reader |
| powerful, seamless, blazing fast, robust, beautiful | What it does, with a number if there is one |
| you may use, you are able to | use, or you can use |
| in order to | to |
| note that, it is important to note | Nothing: state the fact, or use a `NOTE` |
| please | Nothing |
| will, for what always happens | The present tense |

Before finishing, grep for them and judge each hit ("just after" and "Let's Encrypt" are fine):

```sh
grep -nwiE 'simply|just|easily|obviously|of course|please|you may|we|let.s|our|powerful|seamless|in order to' docs/*.md site/content/docs/*.md README.md
```

## Code blocks

- A lead-in sentence ending in a colon comes before every block. The only exception is a command synopsis directly under a reference heading.
- Every block names its language: `sh`, `yaml`, `json`, `caddyfile`, or `text` for output, synopses and DNS records.
- Commands can be pasted as they are: no `$` prompt, one command per line, long commands split with `\`.
- Output goes in a trailing comment (`# prints https://hello.art.example.com/`), or in a `text` block introduced by "It prints:".
- In a block of variants, put a comment above each one saying how it differs.
- Configuration excerpts show only the keys that matter, and the lead-in says so ("The settings that matter are:").
- Keep one running example through a page, using names the docs already use: `art.example.com`, the slugs `q3-report` and `hello`, keys written `kzk_...`, addresses from `203.0.113.0/24` and `198.51.100.0/24`, and mail at `example.com`. Angle-bracket placeholders (`<slug>`) are for synopses only. Never `foo` or `bar`.

## Lists and tables

- Use a numbered list for steps in order and bullets for parallel items. Start every item in a list the same way: all nouns, or all imperatives.
- Use a table to look up the same facts about many items, such as flags, keys, tools or exit codes. Keep each cell to a phrase or one sentence. An item that needs more gets an H4 with prose and an example instead.
- A list or table gets a lead-in sentence too.

## Callouts

GitHub and the site both render GitHub alerts, so pages in either place can use them:

- `> [!NOTE]` for something useful that a reader can skip, such as an alternative or a platform difference.
- `> [!TIP]` for a shortcut. Use it rarely.
- `> [!WARNING]` for data loss, a security exposure or downtime. Say what goes wrong and how to avoid it.

Put a callout right after the step it concerns. Use one or two per section at most, never two in a row, and no bold inside. Anything the reader must do is a step, not a callout:

```markdown
> [!WARNING]
> `app restore` replaces the live database. Stop the server first, or writes made during the restore are lost.
```

## Links

- Link at the first mention of anything documented elsewhere, with link text that names it: `[share links](api.md#share-links)`, not `see [api.md](api.md)`.
- Each fact has one home page (the `docs-check` skill lists them). State it in full there, and elsewhere give one sentence and a link. Never copy a paragraph onto a second page.
- No links in headings.

## Left out on purpose

The Laravel docs also do these things. Kazari's docs do not:

- **Selling**: "Why X?" sections, adjectives, and plugs for a hosted service. The landing page sells; a docs page explains.
- **Minimisers** such as "simply", "of course" and "a breeze", which are wrong whenever the step fails.
- **"You may" on every option** (about one word in a hundred there), plus "we" and "let's".
- **Hand-written contents lists and custom anchors**, which drift away from the headings they point at.
- **The same paragraph on two pages**, where the copies drift apart.
- **Skipped heading levels**, such as an H4 straight under an H2.
- **Pages that mix a walkthrough with a long reference.** Split them: the guide does the task and links to the reference for the rest.

## Before finishing

- The lede says what the subject is, why it matters, what the page covers and what comes first.
- Sections follow the order of use, with the common case before the options.
- Every code block has a lead-in and, unless the result is obvious, a sentence on the result.
- No paragraph runs past three sentences, and every hit from the grep above is justified.
- Defaults are stated in words, and each warning sits next to its step.
- Link texts describe their targets, and no link still points at the old anchor of a renamed heading.
- The mechanics in `.claude/rules/docs.md` hold, and `hugo --source site --gc --minify` prints no WARN or ERROR.
