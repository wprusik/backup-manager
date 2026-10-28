param(
    [ValidateSet('all', 'amd64', 'arm64')]
    [string]$Arch = 'all'
)

$ErrorActionPreference = 'Stop'

$goCommand = Get-Command go -CommandType Application -ErrorAction SilentlyContinue
$goPath = if ($goCommand) { $goCommand.Source } else { $null }
if (-not $goPath) {
    $sdkRoot = Join-Path $env:USERPROFILE 'sdk'
    $sdk = Get-ChildItem -LiteralPath $sdkRoot -Directory -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match '^go\d+\.\d+(\.\d+)?$' -and (Test-Path -LiteralPath (Join-Path $_.FullName 'bin\go.exe')) } |
        Sort-Object { [version]$_.Name.Substring(2) } -Descending |
        Select-Object -First 1
    if ($sdk) { $goPath = Join-Path $sdk.FullName 'bin\go.exe' }
}
if (-not $goPath) {
    throw 'Go not found. Install Go >= 1.23 and add it to PATH, or install an SDK under ~/sdk/goVERSION.'
}

$savedEnvironment = @{}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

Push-Location -LiteralPath $PSScriptRoot
try {
    $env:GOOS = 'linux'
    $env:CGO_ENABLED = '0'
    $architectures = if ($Arch -eq 'all') { @('amd64', 'arm64') } else { @($Arch) }
    New-Item -ItemType Directory -Force -Path (Join-Path $PSScriptRoot 'dist') | Out-Null
    foreach ($architecture in $architectures) {
        $env:GOARCH = $architecture
        $outputPath = Join-Path $PSScriptRoot "dist/backup-manager-linux-$architecture"
        Write-Host "Building linux/$architecture..."
        & $goPath build -trimpath -o $outputPath .
        if ($LASTEXITCODE -ne 0) { throw "Build failed for linux/$architecture (exit $LASTEXITCODE)." }
        Write-Host "Built: $outputPath"
    }
}
finally {
    foreach ($name in $savedEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
    Pop-Location
}
