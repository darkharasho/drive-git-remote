## Install

Download the archive for your platform, unpack it, and put `drive-git` on your PATH:

```sh
tar -xzf drive-git___VERSION___darwin_arm64.tar.gz
mv drive-git ~/.local/bin/
drive-git install-helper   # so `git clone gdrive://name` works
```

Verify a download against `checksums.txt`:

```sh
sha256sum -c checksums.txt --ignore-missing
```

First run: `drive-git setup`, then `drive-git login`.

The same binary serves both the CLI and, invoked as `git-remote-gdrive`, git's
remote helper protocol — `install-helper` just symlinks it under that name.
