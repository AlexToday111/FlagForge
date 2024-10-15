param([switch]$SkipBuild)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
if (Test-Path -LiteralPath '.env') {
    foreach ($line in (Get-Content -LiteralPath '.env')) {
        if ($line -match '^(FLAGFORGE_[A-Z_]+)=(.*)$') {
            if (-not [Environment]::GetEnvironmentVariable($Matches[1])) { [Environment]::SetEnvironmentVariable($Matches[1],$Matches[2],'Process') }
        }
    }
}
if (-not $env:FLAGFORGE_ADMIN_TOKEN) {
    if (Test-Path -LiteralPath '.env') {
        $tokenLine = Get-Content -LiteralPath '.env' | Where-Object { $_ -match '^FLAGFORGE_ADMIN_TOKEN=' } | Select-Object -First 1
        if ($tokenLine) { $env:FLAGFORGE_ADMIN_TOKEN = $tokenLine.Substring('FLAGFORGE_ADMIN_TOKEN='.Length) }
    }
    if (-not $env:FLAGFORGE_ADMIN_TOKEN) { $env:FLAGFORGE_ADMIN_TOKEN = 'api_admin_' + [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N') }
}
if (-not $SkipBuild) { docker compose up -d --build postgres redis server otel; if ($LASTEXITCODE) { throw 'Docker Compose failed' } }
$apiPort = if ($env:FLAGFORGE_API_PORT) { $env:FLAGFORGE_API_PORT } else { '18080' }
$examplePort = if ($env:FLAGFORGE_EXAMPLE_PORT) { $env:FLAGFORGE_EXAMPLE_PORT } else { '18081' }
$api = "http://localhost:$apiPort"
for ($i=0; $i -lt 90; $i++) { try { $null = Invoke-RestMethod "$api/readyz"; break } catch { Start-Sleep -Seconds 1 } }
$headers = @{ Authorization = "Bearer $env:FLAGFORGE_ADMIN_TOKEN" }
function Invoke-Admin($Method, $Path, $Body, $Version) {
    $requestHeaders = $headers.Clone()
    if ($null -ne $Version) { $requestHeaders['If-Match'] = "$Version" }
    $params = @{ Method=$Method; Uri="$api$Path"; Headers=$requestHeaders; ContentType='application/json' }
    if ($null -ne $Body) { $params.Body = $Body | ConvertTo-Json -Depth 30 -Compress }
    Invoke-RestMethod @params
}
$project = 'demo-' + [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$null = Invoke-Admin 'POST' '/api/v1/projects' @{key=$project;name='Demo'}
$environment = Invoke-Admin 'POST' "/api/v1/projects/$project/environments" @{key='production';name='Production'}
$env:FLAGFORGE_SDK_KEY = $environment.sdk_key
$taskEnvLines = if (Test-Path -LiteralPath '.env') { @(Get-Content -LiteralPath '.env' | Where-Object { $_ -notmatch '^FLAGFORGE_(SDK_KEY|DEMO_PROJECT)=' }) } else { @("FLAGFORGE_ADMIN_TOKEN=$env:FLAGFORGE_ADMIN_TOKEN") }
[System.IO.File]::WriteAllLines((Join-Path $PWD '.env'), @($taskEnvLines) + @("FLAGFORGE_SDK_KEY=$env:FLAGFORGE_SDK_KEY", "FLAGFORGE_DEMO_PROJECT=$project"))
$null = Invoke-Admin 'POST' "/api/v1/projects/$project/flags" @{key='new-checkout';name='New checkout';type='BOOLEAN'}
$path = "/api/v1/projects/$project/environments/production/flags/new-checkout"
$null = Invoke-Admin 'PUT' $path @{type='BOOLEAN';enabled=$false;variations=@{on=$true;off=$false};default_variation='off';off_variation='off'} 0
if ($SkipBuild) { docker compose --profile demo up -d --no-build example } else { docker compose --profile demo up -d --build example }
if ($LASTEXITCODE) { throw 'Example startup failed' }
function Wait-Version($Version) {
    for ($i=0;$i -lt 150;$i++) {
        try { $result = Invoke-RestMethod "http://localhost:$examplePort/checkout?user=user-1"; if ($result.sdk.version -ge $Version) { return $result } } catch {}
        Start-Sleep -Milliseconds 100
    }
    throw "SDK propagation timeout for version $Version"
}
$initial = Wait-Version 1
if ($initial.checkout -ne 'old') { throw 'Initial checkout should be old' }
Write-Output 'Initial checkout: old'
foreach ($percentage in @(10,50)) {
    $current = Invoke-Admin 'GET' $path
    $version = $current.version
    $current.enabled = $true
    $current | Add-Member -MemberType NoteProperty -Name rollout -Value @(@{variation_key='on';weight=$percentage*1000},@{variation_key='off';weight=100000-$percentage*1000}) -Force
    $updated = Invoke-Admin 'PUT' $path $current $version
    $null = Wait-Version $updated.version
    $new = 0
    for ($i=1;$i -le 100;$i++) { $result = Invoke-RestMethod "http://localhost:$examplePort/checkout?user=user-$i"; if ($result.checkout -eq 'new') { $new++ } }
    Write-Output "$percentage% rollout: $new of 100 deterministic users saw new checkout"
}
$current = Invoke-Admin 'GET' $path
$disabled = Invoke-Admin 'POST' "$path/disable" $null $current.version
$result = Wait-Version $disabled.version
if ($result.checkout -ne 'old') { throw 'Kill switch failed' }
Write-Output "Kill switch propagated, SDK version $($result.sdk.version)"
Write-Output "Project: $project"
Write-Output 'Services remain running. Stop them with docker compose --profile demo --profile multi down.'
