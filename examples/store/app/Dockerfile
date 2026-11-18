# One image for the three services of the store; ROLE (web, api or worker) picks the part.
FROM node:22-alpine
WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund && npm cache clean --force
COPY src ./src
COPY public ./public
ENV NODE_ENV=production PORT=8080
EXPOSE 8080
USER node
CMD ["node", "src/main.js"]
