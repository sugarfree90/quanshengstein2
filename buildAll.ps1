$AppName = "catWebservice"
$OutDir = "build"

if (-not (Test-Path $OutDir)) {
    New-Item -ItemType Directory -Path $OutDir | Out-Null
}

$Targets = @(
    @{ OS="windows"; Arch="amd64" },
    @{ OS="windows"; Arch="386" },
    @{ OS="windows"; Arch="arm64" },
    @{ OS="linux"; Arch="amd64" },
    @{ OS="linux"; Arch="386" },
    @{ OS="linux"; Arch="arm64" },
    @{ OS="linux"; Arch="arm"; ArmVer="7" },
    @{ OS="linux"; Arch="arm"; ArmVer="6" },
    @{ OS="linux"; Arch="arm"; ArmVer="5" }
)

$GoCmd = "go"
if (-not (Get-Command "go" -ErrorAction SilentlyContinue)) {
    if (Test-Path "C:\Program Files\Go\bin\go.exe") {
        $GoCmd = "C:\Program Files\Go\bin\go.exe"
    } elseif (Test-Path "C:\Go\bin\go.exe") {
        $GoCmd = "C:\Go\bin\go.exe"
    }
}

Write-Host "Starting cross-compilation with $GoCmd..." -ForegroundColor Cyan

foreach ($Target in $Targets) {
    $env:GOOS = $Target.OS
    $env:GOARCH = $Target.Arch
    
    # Proper string interpolation in PowerShell
    $Suffix = "$($Target.OS)_$($Target.Arch)"
    
    if ($Target.ContainsKey("ArmVer")) {
        $env:GOARM = $Target.ArmVer
        $Suffix += "v$($Target.ArmVer)"
    } else {
        Remove-Item Env:\GOARM -ErrorAction SilentlyContinue
    }

    $Extension = ""
    if ($Target.OS -eq "windows") {
        $Extension = ".exe"
    }

    $OutputFile = Join-Path $OutDir "${AppName}_${Suffix}${Extension}"

    $GoArmInfo = if ($Target.ContainsKey("ArmVer")) { " (GOARM=$($Target.ArmVer))" } else { "" }
    Write-Host "Building: $($Target.OS) / $($Target.Arch)$GoArmInfo -> $OutputFile"

    & $GoCmd build -o $OutputFile

    if ($LASTEXITCODE -ne 0) {
        Write-Host "Error building for $($Target.OS)/$($Target.Arch)$GoArmInfo" -ForegroundColor Red
    }
}

Write-Host "Compilation complete. Output files located in '$OutDir'." -ForegroundColor Green

Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\GOARM -ErrorAction SilentlyContinue