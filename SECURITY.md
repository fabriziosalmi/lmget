# Security Policy

lmget downloads files that other programs load and run: model weights, tokenizers, chat templates. It verifies every file against the hash its source publishes, refuses files whose content does not match their name, and treats lock files from cloned repositories as untrusted input. A way around any of these checks is a security issue.

## Supported versions

Only the latest release receives security fixes. lmget is in early development (0.x): upgrade before reporting.

## Reporting a vulnerability

Do not open a public issue or pull request for a security report.

Open a private security advisory: <https://github.com/fabriziosalmi/lmget/security/advisories/new>

Please include the lmget version (`lmget --version`), the operating system, the command, and the smallest reproduction you have (a lock file, a fake server, a file).

## What happens next

1. Acknowledgement within 7 days.
2. A fix is developed in a private fork, with a regression test.
3. A release with the fix, then the advisory is published with credit to the reporter, unless they prefer otherwise.

## Scope

In scope: hash verification, content checks (pickle and format detection), lock file handling, paths lmget writes or deletes (store, Hugging Face cache, Ollama's models directory, export directories), credentials (the Hugging Face token), and the release artifacts and their provenance.

Out of scope: the content of the models themselves, and vulnerabilities in the sources lmget downloads from (Hugging Face, the Ollama registry, other hosts).
