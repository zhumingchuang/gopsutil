[CmdletBinding()]
param(
    [switch]$BuildOnly,
    [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
$collectorRoot = Split-Path -Parent $PSScriptRoot
$collectorOutput = if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    Join-Path $collectorRoot 'build/render-resources'
} else {
    if (-not [System.IO.Path]::IsPathRooted($OutputDirectory)) { throw 'Collector output directory must be absolute' }
    [System.IO.Path]::GetFullPath($OutputDirectory)
}
$collectorDll = Join-Path $collectorOutput 'render_resources.dll'
function Get-CollectorHash([string]$Path) {
    $stream = [System.IO.File]::OpenRead($Path)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { return ([System.BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-', '').ToLowerInvariant() }
    finally { $stream.Dispose(); $sha.Dispose() }
}

if (-not $IsWindows -and $env:OS -ne 'Windows_NT') { throw 'Windows build host required' }
Push-Location -LiteralPath $collectorRoot
try {
    $collectorTarget = (& go env GOOS GOARCH CGO_ENABLED) -join '/'
    if ($LASTEXITCODE -ne 0 -or $collectorTarget -ne 'windows/amd64/1') {
        throw 'render_resources.dll requires GOOS=windows, GOARCH=amd64, CGO_ENABLED=1'
    }
    New-Item -ItemType Directory -Path $collectorOutput -Force | Out-Null
    if (-not $BuildOnly) {
        & go test ./internal/resourcecollector ./cmd/render-resources
        if ($LASTEXITCODE -ne 0) { throw 'Resource collector tests failed' }
    }
    & go build -trimpath -buildvcs=true -buildmode=c-shared -o $collectorDll ./cmd/render-resources
    if ($LASTEXITCODE -ne 0) { throw 'Resource collector DLL build failed' }
    $collectorGoVersion = & go version
    $collectorRevision = & git rev-parse HEAD
    if ($LASTEXITCODE -ne 0) { throw 'Source revision unavailable' }
    $collectorDirty = [bool](& git status --porcelain)
    $collectorInputs = @(& git ls-files --cached --others --exclude-standard) |
        Where-Object { $_ -match '\.(go|c|h)$|^go\.(mod|sum)$|^LICENSE$|^scripts/build-render-resources\.ps1$' } |
        Sort-Object -Unique
    if ($LASTEXITCODE -ne 0) { throw 'Cannot enumerate source inputs' }
    $collectorHashes = @($collectorInputs | ForEach-Object {
        [ordered]@{ path = $_; sha256 = (Get-CollectorHash (Join-Path $collectorRoot $_)) }
    })
    [ordered]@{
        abiVersion = 1
        target = 'windows/amd64'
        buildOnly = [bool]$BuildOnly
        testsRun = (-not $BuildOnly)
        goVersion = $collectorGoVersion
        sourceRevision = $collectorRevision
        sourceDirty = $collectorDirty
        dllSha256 = (Get-CollectorHash $collectorDll)
        headerSha256 = (Get-CollectorHash (Join-Path $collectorOutput 'render_resources.h'))
        sourceFiles = $collectorHashes
    } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $collectorOutput 'build-manifest.json') -Encoding utf8
    Write-Output "Built $collectorDll"
} finally { Pop-Location }
