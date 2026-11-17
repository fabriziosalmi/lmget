# lmget installer for Windows: https://lmget.org/install.ps1
#
#   irm https://lmget.org/install.ps1 | iex
#
# Downloads the latest lmget release for this CPU from GitHub, checks the
# archive against the release's checksums.txt and, when the GitHub CLI (gh)
# is installed and signed in, its build provenance attestation too. Then it
# installs lmget.exe into $env:LMGET_INSTALL_DIR (default
# %LOCALAPPDATA%\Programs\lmget). Nothing else is written, no administrator
# rights are needed, and your PATH is not changed: the script says how.
#
# LMGET_VERSION=v0.3.0 installs a given release. LMGET_RELEASE_URL takes the
# archives from another server (a mirror, or a test) and needs LMGET_VERSION;
# checksums.txt then comes from that server too, so only the attestation
# check (gh) proves where the archive was built.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar slows Invoke-WebRequest down
$repo = 'fabriziosalmi/lmget'

function Say([string]$m) { [Console]::Error.WriteLine("lmget-install: $m") }
# throw, never exit: run through "| iex", exit would close the user's window
function Die([string]$m) { throw "lmget-install: $m" }

$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
	'AMD64' { $arch = 'amd64' }
	'ARM64' { $arch = 'arm64' }
	default { Die "no release for $cpu`: build from source, see https://github.com/$repo" }
}

$tag = $env:LMGET_VERSION
if (-not $tag) {
	if ($env:LMGET_RELEASE_URL) { Die 'LMGET_RELEASE_URL needs LMGET_VERSION' }
	# the latest release page redirects to its tag: no API call, no rate limit
	$r = Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest" -UseBasicParsing
	if ($r.BaseResponse.ResponseUri) { $u = $r.BaseResponse.ResponseUri.AbsoluteUri } # Windows PowerShell 5.1
	else { $u = $r.BaseResponse.RequestMessage.RequestUri.AbsoluteUri } # PowerShell 7
	$tag = $u.TrimEnd('/').Split('/')[-1]
}
if ($tag -notmatch '^v[0-9][0-9A-Za-z.+-]*$') { Die "unexpected release tag: $tag" }

$name = "lmget_$($tag.Substring(1))_windows_$arch"
$archive = "$name.tar.gz"
if ($env:LMGET_RELEASE_URL) { $base = $env:LMGET_RELEASE_URL.TrimEnd('/') }
else { $base = "https://github.com/$repo/releases/download/$tag" }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('lmget-install-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
	Say "downloading $archive ($tag)"
	Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive) -UseBasicParsing
	Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

	$want = $null
	foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
		$f = $line -split '\s+'
		if ($f.Count -ge 2 -and $f[1] -eq $archive) { $want = $f[0].ToLowerInvariant(); break }
	}
	if (-not $want) { Die "$archive is not listed in checksums.txt" }
	$got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLowerInvariant()
	if ($got -ne $want) { Die "sha256 mismatch for $archive`: expected $want, got $got" }
	Say 'sha256 matches checksums.txt'

	# gh needs to be signed in to fetch the attestation: a gh that is not is
	# treated like a missing one
	$gh = Get-Command gh -ErrorAction SilentlyContinue
	$signedIn = $false
	if ($gh) { & gh auth status *> $null; $signedIn = ($LASTEXITCODE -eq 0) }
	if ($signedIn) {
		& gh attestation verify (Join-Path $tmp $archive) --repo $repo `
			--signer-workflow "$repo/.github/workflows/release.yml" --source-ref "refs/tags/$tag" *> $null
		if ($LASTEXITCODE -ne 0) { Die 'the build provenance attestation does not verify: not installing' }
		Say "build provenance verified: built by release.yml from $tag"
	} elseif ($gh) {
		Say "gh is not signed in: provenance not checked. To check it: gh auth login, then gh attestation verify $archive --repo $repo"
	} else {
		Say "gh not found: provenance not checked. To check it: gh attestation verify $archive --repo $repo"
	}

	& tar -xzf (Join-Path $tmp $archive) -C $tmp
	if ($LASTEXITCODE -ne 0) { Die "cannot extract $archive (tar comes with Windows 10 1803 and later)" }
	$exe = Join-Path $tmp "$name\lmget.exe"
	if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { Die 'the archive holds no lmget.exe' }

	if ($env:LMGET_INSTALL_DIR) { $dir = $env:LMGET_INSTALL_DIR }
	else { $dir = Join-Path $env:LOCALAPPDATA 'Programs\lmget' }
	New-Item -ItemType Directory -Force -Path $dir | Out-Null
	$dst = Join-Path $dir 'lmget.exe'
	Copy-Item -LiteralPath $exe -Destination "$dst.new" -Force
	Move-Item -LiteralPath "$dst.new" -Destination $dst -Force
	$version = & $dst --version
	Say "installed $version at $dst"

	$inPath = ($env:Path -split ';') -contains $dir
	if (-not $inPath) {
		Say "add it to your PATH, for example: [Environment]::SetEnvironmentVariable('Path', '$dir;' + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')"
	}
} finally {
	Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
