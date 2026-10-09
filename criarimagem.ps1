$ErrorActionPreference = "Continue"
$PSNativeCommandUseErrorActionPreference = $false

$IMAGE_NAME = "whazing/wuzapi:latest"
$BUILDER_NAME = "whazing-builder"

Write-Host "Verificando se builder '$BUILDER_NAME' existe..."

docker buildx inspect $BUILDER_NAME *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Builder nao existe. Criando builder '$BUILDER_NAME'..."
    docker buildx create --name $BUILDER_NAME --driver docker-container --use 2>&1 | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit 1 }
} else {
    Write-Host "Builder '$BUILDER_NAME' ja existe. Usando ele."
    docker buildx use $BUILDER_NAME 2>&1 | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit 1 }
}

Write-Host "Iniciando build multiplataforma e push da imagem $IMAGE_NAME..."

docker buildx build `
  --platform linux/amd64,linux/arm64 `
  -t $IMAGE_NAME `
  --push . 2>&1 | ForEach-Object { Write-Host $_ }

if ($LASTEXITCODE -ne 0) {
    Write-Host "Erro no build/push."
    exit 1
}

Write-Host "Build e push concluidos com sucesso!"
