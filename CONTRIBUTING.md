# Contributing

> **Classification:** Current — How to take part in maverickbuilds.app from outside the team.

Thank you for looking. Everyone taking part follows the
[code of conduct](CODE_OF_CONDUCT.md). This repository is published as **snapshots**: the
product is developed in a separate repository, and every commit here
(`Snapshot <id> (<date>)`) replaces the whole tree with the state of one
commit there. That shapes what you can send us.

## Where things go

| You want to | Go to |
| --- | --- |
| Report a bug or a documentation error | [Issues](https://github.com/Mavericka-SARL/maverick-builds-app/issues/new/choose), bug report form |
| Ask for a feature you know you need | [Issues](https://github.com/Mavericka-SARL/maverick-builds-app/issues/new/choose), feature request form |
| Float an idea, or upvote someone else's | [Discussions › Ideas](https://github.com/Mavericka-SARL/maverick-builds-app/discussions/categories/ideas) |
| Share a model you built | [Discussions › Show and tell](https://github.com/Mavericka-SARL/maverick-builds-app/discussions/categories/show-and-tell) |
| Ask how to install, configure or use it | [Discussions › Q&A](https://github.com/Mavericka-SARL/maverick-builds-app/discussions/categories/q-a) |
| Report a security problem | Privately, as [SECURITY.md](SECURITY.md) explains, never in public |

## Issues: bugs and feature requests

A useful bug report says:

- which release you run (or the commit here, if you built from main) and how
  you deployed it (Docker Compose, Kubernetes, local development);
- what you did, what you expected, and what happened instead;
- logs or a screenshot where they help. Remove passwords, tokens and licence
  keys first.

A feature request starts from the problem: who needs it (developer,
administrator, business user) and what they cannot do today.

## Sharing models

A model you built can be exported as a package and imported by anyone else
running maverickbuilds.app. [examples/README.md](examples/README.md) explains
how to export one **definitions only** (no values, no form records), what to
check before you post it, and how to import one. The sales-planning model
there is a ready-made example.

## Roadmap and releases

Ideas with the most upvotes become issues. Issues we have taken on are on the
[roadmap board](https://github.com/orgs/Mavericka-SARL/projects/1), grouped into to do, in progress and done,
so you can see whether yours is coming. Every
[release](https://github.com/Mavericka-SARL/maverick-builds-app/releases)
lists the issues it closed.

## Pull requests: not yet

We cannot merge outside code at the moment. The same code base is offered
under the [Sustainable Use License](LICENSE) and under commercial and
enterprise licences (see [docs/LICENSING.md](docs/LICENSING.md)), so we can
only take a contribution under a contributor licence agreement, and that
agreement does not exist yet. A pull request would also be overwritten by the
next snapshot.

If you have a fix, open an issue describing it, or link a branch in your fork
from the issue. We may implement it ourselves, and we will say so in the
issue. This section changes once the agreement is in place.

## Contact

For anything that fits neither an issue nor a security report:
team@maverickans.com.
