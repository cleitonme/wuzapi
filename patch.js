const fs = require('fs');
const path = require('path');

const handlersPath = path.join(__dirname, 'handlers.go');
const newHandlersPath = path.join(__dirname, 'new_handlers.txt');

try {
    let handlersContent = fs.readFileSync(handlersPath, 'utf8');
    const newHandlersContent = fs.readFileSync(newHandlersPath, 'utf8');

    // Regex para encontrar o bloco
    // Começa em: // Sends Buttons (not implemented, does not work)
    // Termina em: // Sends a status text message

    // Nota: O comentário pode ter mudado em versões, então vamos ser flexíveis nos espaços
    // Procurar por func (s *server) SendButtons e ir até func (s *server) SetStatusMessage

    const startMarker = "// Sends Buttons (not implemented, does not work)";
    const endMarker = "// Sends a status text message";

    const startIndex = handlersContent.indexOf(startMarker);
    const endIndex = handlersContent.indexOf(endMarker);

    if (startIndex !== -1 && endIndex !== -1) {
        console.log(`Bloco encontrado! Start: ${startIndex}, End: ${endIndex}`);

        const before = handlersContent.substring(0, startIndex);
        const after = handlersContent.substring(endIndex);

        const updatedContent = before + newHandlersContent + "\n\n" + after;

        fs.writeFileSync(handlersPath, updatedContent, 'utf8');
        console.log("handlers.go atualizado com sucesso!");
    } else {
        console.error("ERRO: Marcadores não encontrados.");
        console.log(`Start Marker (${startMarker}): ${startIndex}`);
        console.log(`End Marker (${endMarker}): ${endIndex}`);
        process.exit(1);
    }
} catch (e) {
    console.error("Erro ao processar arquivos:", e);
    process.exit(1);
}
