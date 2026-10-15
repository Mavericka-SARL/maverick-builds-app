# Contributing

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
how to export one **definitions only** (no values, no records), what to
check before you post it, and how to import one. The sales-planning model
there is a ready-made example.

## Roadmap and releases

Ideas with the most upvotes become issues. Issues we have taken on are on the
[roadmap board](https://github.com/orgs/Mavericka-SARL/projects/1), grouped into to do, in progress and done,
so you can see whether yours is coming. Every
[release](https://github.com/Mavericka-SARL/maverick-builds-app/releases)
lists the issues it closed.

## Code contributions

A fix or an improvement is welcome. Because this repository is published as
snapshots, a pull request cannot be merged as it stands: we apply the change in
the development repository, and it reaches this one with the next snapshot,
where the issue or pull request names it. Open a pull request, or an issue that
links a branch in your fork, and say in it that you accept the contributor terms
below. We take a contribution only under those terms.

### Contributor terms

The same code base is offered under the [Sustainable Use License](LICENSE), the
[Enterprise Features License](ee/LICENSE) and commercial agreements (see
[docs/LICENSING.md](docs/LICENSING.md)), so a contribution has to be usable
under all of them. By sending one, as a pull request, a patch, or code in an
issue, a discussion or an e-mail, you agree that:

1. you wrote it, or otherwise have the right to send it under these terms, and
   if your employer has rights in your work, it has agreed;
2. you keep the copyright in it, and you grant Mavericka SARL-S, the owner of
   maverickbuilds.app, a perpetual, worldwide, non-exclusive, royalty-free and
   irrevocable license to use, copy, modify, distribute and sublicense it, and
   to license it under any terms, including the licenses above;
3. you grant Mavericka SARL-S the same license under any patent claims you can
   license that your contribution, alone or combined with maverickbuilds.app,
   necessarily infringes;
4. you tell us in the contribution about any part of it that is someone
   else's work, and under which license; and
5. Mavericka SARL-S may decide not to use it, and owes nothing for it.

## Contact

For anything that fits neither an issue nor a security report:
team@maverickans.com.

maverickbuilds.app is developed and published by Mavericka SARL-S,
2 Rue Pierre-Joseph Redouté, L-2435 Luxembourg (RCS Luxembourg B289552).
