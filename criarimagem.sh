#!/bin/bash

#
# Script para build e push de imagem Docker multiplataforma
# Compatível com linux/amd64 e linux/arm64
#

set -e

# Configurações
IMAGE_NAME="whazing/wuzapi:latest"
BUILDER_NAME="wuzapi-builder"

echo "🚀 Verificando se builder '$BUILDER_NAME' existe..."

if ! docker buildx inspect $BUILDER_NAME > /dev/null 2>&1; then
  echo "➕ Builder não existe. Criando builder '$BUILDER_NAME'..."
  docker buildx create --name $BUILDER_NAME --driver docker-container --use
  docker buildx inspect --bootstrap
else
  echo "✅ Builder '$BUILDER_NAME' já existe. Usando ele."
  docker buildx use $BUILDER_NAME
fi

echo "📦 Iniciando build multiplataforma e push da imagem $IMAGE_NAME..."

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t $IMAGE_NAME \
  --push .

echo "🎉 Build e push concluídos com sucesso!"
echo "📌 Imagem disponível em: $IMAGE_NAME"