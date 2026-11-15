"""brew-formula.py VERSION checksums.txt > lmget.rb

Writes the Homebrew formula for the fabriziosalmi/homebrew-lmget tap from the
checksums of a release, so the formula and the release cannot disagree.
"""
import re
import sys

version, checksums = sys.argv[1], sys.argv[2]
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", version):
    sys.exit("version must look like v0.1.0")
v = version[1:]
sums = {}
for line in open(checksums):
    digest, name = line.split()
    # the digest lands inside Ruby code: only a sha256 in hex may get there
    if not re.fullmatch(r"[0-9a-f]{64}", digest) or name in sums:
        sys.exit(f"{checksums}: bad or repeated line for {name!r}")
    sums[name] = digest
base = f"https://github.com/fabriziosalmi/lmget/releases/download/{version}"


def asset(os_, arch):
    name = f"lmget_{v}_{os_}_{arch}.tar.gz"
    if name not in sums:
        sys.exit(f"{name} is not in {checksums}")
    return f'      url "{base}/{name}"\n      sha256 "{sums[name]}"'


print(f'''class Lmget < Formula
  desc "Get open models from their sources, verified"
  homepage "https://github.com/fabriziosalmi/lmget"
  version "{v}"
  license "Apache-2.0"

  on_macos do
    on_arm do
{asset("darwin", "arm64")}
    end
    on_intel do
{asset("darwin", "amd64")}
    end
  end

  on_linux do
    on_arm do
{asset("linux", "arm64")}
    end
    on_intel do
{asset("linux", "amd64")}
    end
  end

  def install
    bin.install "lmget"
  end

  test do
    assert_match "lmget v#{{version}}", shell_output("#{{bin}}/lmget --version")
  end
end''')
