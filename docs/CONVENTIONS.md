# Conventions

Rules for changing this repository's documentation and for the claims it makes. Code conventions are
the Go defaults plus `go vet`; these are the ones a linter cannot enforce and that have been gotten
wrong before.

## Accuracy guards

Claims in the docs must match the code and the measurements. These have each been wrong at some point.

**Quote the paraphrased phrasing, and say so.** `eval/golden.json` carries two phrasings per question.
The identifier phrasing leaks the answer's distinctive tokens into the query and scores higher for
that reason alone, which is precisely the finding that had to be retracted once. Numbers in the README
and in `eval/README.md` are the paraphrased set unless a sentence says otherwise.

**No score is quoted before the judge is validated.** `eval/cmd/judge -validate` grades deliberately
broken answers and exits non-zero if it cannot separate them from good ones. A faithfulness ceiling of
2.00 is only readable as a result because that check passes; without it, a grader that scores
everything highly is indistinguishable from a service that answers everything well. Never quote a
judge number from a run that skipped validation.

**One question is 2.9 points.** With 35 questions, any delta under about 3 points is a single question
changing its mind. Do not describe a sub-3-point movement as an improvement or a regression, and do not
add a decimal place to imply resolution the sample size does not have.

**Severed-boundary figures are paired with their configuration.** 66% is fixed-size chunking at the
800-rune ceiling, which is the same-ceiling control and therefore the only honest comparison against
structure-aware chunking's 4.8%. The 70% figure describes the original fixed-500 configuration and is
not the right baseline. This was published wrong once.

**The deployed tasks run in public subnets.** Express Mode uses one subnet set for both the load
balancer and the tasks. The private app subnets and the NAT gateway are provisioned but off the request
path. Any sentence implying the tasks are network-isolated is false; the accurate claim is that they
are unreachable except through the load balancer, because the app security group has no inbound rule
of its own.

**Nothing writes to the S3 bucket.** `infra/s3.tf` and its task-role grant exist so raw file upload can
be added without another infra change. Do not describe document storage as a feature.

**"Deployed" is accurate; a live URL is not.** The service has been deployed end to end and has served
grounded answers from ECS Express Mode on Fargate, which is what the word claims. The billable stack is
destroyed after every session, so there is no permanent public URL and the repository's website field
is deliberately empty. **Any rewrite that keeps the word must keep the caveat with it**, currently at
the close of the README's `## Results`. It was dropped once during a README restructure, which is how
this guard came to exist. Present-tense "the service runs on ECS" is the wrong tense; it has run and
can run again.

## Generated artifacts

`docs/demo.gif` is **generated, not recorded by hand.** A script drives the real service through the
real embedded demo page and captures the result, so the source count and the latency visible in it are
that run's own output.

- Never hand edit it.
- Never describe it with numbers it does not show. The "5 sources in 2447 ms" in both the README's alt
  text **and its caption** comes from the run that produced the current file. Re-record and both have
  to be re-read off the new gif.
- The caption's job is four things, in the reference repo's order: state what is real, explain a
  surprising number the viewer can see, disclose anything configured for the recording, and confirm the
  load-bearing number is not faked. Do not let it drift into describing how the demo page is built;
  that belongs in [API.md](API.md).
- A change to `internal/handler/index.html` makes it stale, and nothing in the tree will say so.
- The generator lives in `content/demo-recording/`, which is gitignored. If it is unavailable, flag the
  gif as stale rather than editing the caption to match a UI the image no longer depicts.

## Documentation layout

The docs are split by audience. **`README.md` is the overview and stays short.** Depth lives here:

| File | Scope |
|---|---|
| `docs/ARCHITECTURE.md` | The two request paths, the four design ideas, the interface boundaries, how evaluation plugs in |
| `docs/API.md` | Endpoint reference, request shapes, status codes, chunking behavior at ingest, what `sources[]` guarantees |
| `docs/LOCAL_DEV.md` | Local run, full environment variable reference, development commands, the evaluation harness |
| `docs/DEPLOYMENT.md` | What gets provisioned on AWS, the topology diagram, both Terraform stacks, step by step deploy, teardown, troubleshooting |
| `docs/OPERATIONS.md` | Running the stack, verifying a deploy, cost, failure modes and the command that diagnoses each, honest constraints |
| `docs/CONVENTIONS.md` | This file |

**Architecture is the code; deployment is the cloud.** `ARCHITECTURE.md` covers the request paths and
the design ideas, and its section headings mirror the README's `How it works` lead-ins one for one, so
a reader who wants depth on an idea knows where it continues. Cloud topology is not architecture in
this split: it belongs to `DEPLOYMENT.md`, and `ARCHITECTURE.md` must not reacquire it.

**The AWS topology diagram is duplicated on purpose, in exactly two places:** `README.md` under
`How it works`, and `DEPLOYMENT.md` under `What gets provisioned`. The README needs it because a
reviewer who reads only that file should still see where the thing runs; `DEPLOYMENT.md` needs it
because a reference doc must stand alone. **Edit both or neither.** It is 19 lines and the copies are
byte-identical, so a diff of the two mermaid blocks is the check. Do not add a third copy.

`eval/README.md` sits outside that table on purpose. It is the measurement record rather than
documentation of the service, and it is the one place where a rejected or retracted result is written
up in full.

**When a README section grows past a few paragraphs, move it into the matching file above and leave a
link.** Do not let the README reabsorb depth.

## The README spine

`README.md` follows a fixed section order, shared across the portfolio repos so they read as one body
of work. Do not rename or reorder these, and do not insert new top-level sections between them:

```
title + badges + what it is → Contents → Demo → The problem → How it works → Quickstart
→ Trade-offs → Results → What I'd do differently → Known gaps and next steps
→ Repo layout → Documentation → License
```

The narrative arc under those names is **problem → approach → trade-offs → results → hindsight**. A
section that does not advance that arc belongs in `docs/`.

Three rules specific to the spine:

- **`Results` ships only if it contains a number a reader could reproduce with a command.** Here that
  command is the `eval-*` Makefile targets. Estimates and derived arithmetic are prose, not table rows.
- **`What I'd do differently` is hindsight; `Known gaps and next steps` is scope.** They are different
  claims and must not be merged. Folding a deliberate scoping call into the hindsight section turns a
  defensible decision into an apparent regret, and the reverse hides a real mistake behind "out of
  scope."
- **`Repo layout` is a table, not an ASCII tree.** The tree wastes horizontal space on box drawing and
  cannot hold a full sentence per entry.

## Writing rules

- **Never link the README or `docs/` to anything in `content/`.** That directory is gitignored, so
  those links 404 for anyone reading the repo on GitHub. `content/` holds unpublished drafts only and
  is never staged or committed.
- **Diagrams are single-direction with no back edges.** Mermaid renders as spaghetti once an edge
  points backwards. If a diagram needs to show two concerns, make it two diagrams.
- **No `classDef`, `style`, or `linkStyle` blocks in any diagram, without exception.** Mermaid on
  GitHub inherits the reader's light or dark theme; hardcoded fills do not, so a palette tuned on a
  white background renders as glaring white boxes for anyone reading in dark mode. Meaning goes in the
  shape and the edge, not the color: `-->` is a request path, `-.->` is an outbound call the service
  makes itself, `[( )]` is a datastore, and every edge carries a label saying what crosses it.
- **A topology diagram draws only what carries traffic**, and says in the prose what it left out. The
  AWS diagram omits the empty private app subnets, the idle NAT gateway, and the second Availability
  Zone. Each omission is named underneath rather than drawn as a greyed-out node, because a node a
  reader has to be told to ignore has already cost them the attention.
- **Security groups go on the edges they permit, never in a box.** A security group is a rule about one
  hop, so `alb -->|"app SG · 8080"| task` states it exactly, and the label is free because the edge is
  already there. Drawing them as bands around the resources they protect is the conventional picture
  and mermaid cannot do it, since subgraphs cannot overlap; that is a reason to move the label, not to
  drop the information. Keep them: on this stack the two SG rules are what make public-subnet tasks
  defensible, so a diagram without them invites exactly the question it should have answered.
- **Nested subgraphs more than one level deep are a smell.** Region inside VPC inside AZ inside subnet
  produces a diagram nobody reads. Two flat subgraphs and labelled edges carry the same information.
- **The name is `rag-api` everywhere**: repo, Go module path, README title, image tag, doc titles.
  **AWS resource names stay `go-rag-api-*`.** They are infrastructure identifiers, renaming them forces
  Terraform to destroy and recreate, and the ECR repository name is a contract shared between
  `infra/bootstrap`, the `data` lookup in `infra/`, and the CI push. The exception covers resource
  names only, never the GitHub identity inside the CI role's trust policy.
- **Verify fast-moving SDK and service shapes against live documentation** rather than memory, then
  write what you verified. Bedrock model IDs and ECS Express Mode's Terraform support have both changed
  under this project.
