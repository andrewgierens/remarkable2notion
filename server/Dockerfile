# The server deployment of the broker. Secrets (NOTION_CLIENT_ID,
# NOTION_CLIENT_SECRET, REDIRECT_URI) are runtime env on the deployment host,
# never baked in here.
FROM node:22-alpine
WORKDIR /app
# No runtime dependencies: the broker is standard-library JavaScript, and
# wrangler is only needed to deploy the Workers variant.
COPY package.json ./
COPY src ./src
USER node
EXPOSE 8080
ENTRYPOINT ["node", "src/node.js"]
