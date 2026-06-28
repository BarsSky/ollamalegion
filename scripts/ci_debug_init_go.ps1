# Debug: simulate Init Go env step on self-hosted runner
$goVersion = "1.25.2"
$goDir = $null

Write-Host "=== Checking Go installation ==="
Write-Host "Current user: $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Write-Host "PATH: $env:Path"
Write-Host "GITHUB_PATH env: '$env:GITHUB_PATH'"
Write-Host "USERPROFILE: '$env:USERPROFILE'"
Write-Host "LOCALAPPDATA: '$env:LOCALAPPDATA'"
Write-Host "ProgramFiles: '$env:ProgramFiles'"
Write-Host "ALLUSERSPROFILE: '$env:ALLUSERSPROFILE'"

Write-Host ""
Write-Host "--- Step 1: Get-Command go.exe ---"
$goExe = Get-Command go.exe -ErrorAction SilentlyContinue
if ($goExe) {
    $goDir = Split-Path -Parent $goExe.Source
    Write-Host "go.exe found in PATH at: $goDir"
} else {
    Write-Host "go.exe NOT found via Get-Command"
}

Write-Host ""
Write-Host "--- Step 2: where.exe go.exe ---"
if (-not $goDir) {
    $whereGo = where.exe go.exe 2>$null
    if ($whereGo) {
        Write-Host "where.exe results:"
        $whereGo | ForEach-Object { Write-Host "  $_" }
        $goDir = Split-Path -Parent ($whereGo | Select-Object -First 1)
        Write-Host "go.exe found via where.exe at: $goDir"
    } else {
        Write-Host "go.exe NOT found via where.exe"
    }
}

Write-Host ""
Write-Host "--- Step 3: Known paths ---"
if (-not $goDir) {
    $candidates = @()
    if (Test-Path env:GOROOT) { $candidates += "$env:GOROOT\bin" }
    $candidates += "$env:ProgramFiles\Go\bin"
    $candidates += "${env:ProgramFiles(x86)}\Go\bin"
    $candidates += "$env:LocalAppData\go\bin"
    $candidates += "$env:ProgramData\chocolatey\bin"
    $candidates += "$env:ALLUSERSPROFILE\chocolatey\bin"
    $candidates += "C:\Go\bin"
    $candidates += "C:\Program Files\Go\bin"
    $candidates += "C:\ProgramData\chocolatey\bin"
    $candidates += "$env:SystemDrive\Go\bin"
    $candidates += "$env:USERPROFILE\scoop\apps\go\current\bin"
    $candidates += "$env:ALLUSERSPROFILE\scoop\apps\go\current\bin"
    foreach ($dir in $candidates) {
        $testPath = "$dir\go.exe"
        if (Test-Path $testPath) {
            $goDir = $dir
            Write-Host "go.exe found at known path: $dir"
            break
        } else {
            Write-Host "  NOT found: $testPath"
        }
    }
}

Write-Host ""
Write-Host "--- Step 4: Recursive search on C:\ ---"
if (-not $goDir) {
    Write-Host "Starting recursive search on C:\... this may take a while..."
    $goPath = where.exe /R C:\ go.exe 2>$null | Select-Object -First 1
    if ($goPath) {
        $goDir = Split-Path -Parent $goPath
        Write-Host "go.exe found via recursive search at: $goDir"
    } else {
        Write-Host "go.exe NOT found via recursive search"
    }
}

Write-Host ""
if (-not $goDir) {
    Write-Host "Go NOT FOUND on system. Would attempt auto-download..."
    Write-Host "Would download: https://go.dev/dl/go$goVersion.windows-amd64.zip"
    Write-Host "Would install to: C:\Go"
    exit 1
} else {
    Write-Host "Go FOUND at: $goDir"
    Write-Host "Go version:"
    & "$goDir\go.exe" version
    if ($LASTEXITCODE -ne 0) {
        Write-Host "::error::Go binary found but go version failed!"
        exit 1
    }
    Write-Host "Go works correctly."
}
