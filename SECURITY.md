# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub private vulnerability
reporting: open the repository's **Security** tab and choose **Report a
vulnerability**. Do not open a public issue or pull request for a security
problem.

Include what you ran, what you expected, what happened, and the version
(`bash-lsp version`). A repository, settings file or script that triggers the
problem is the most useful thing to attach.

## Supported versions

Only the latest release (and the current `main`) receives security fixes.

## Scope

bash-lsp reads the shell files of a workspace and starts a small, fixed set of
programs (ShellCheck, `bash` and `man` for help text, and code-index for
`search`). In scope: a repository's own files (its scripts, `.bash-lsp.toml`,
`.shellcheckrc`, symbolic links and file names) making bash-lsp

- start a program you did not configure or install,
- read files outside the workspace,
- write outside the root (`bash-lsp rename --apply`), or
- leak a secret past redaction.

See "Privacy and security" in the README for the trust model.
