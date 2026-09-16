# 前端镜像（构建产物交由 Nginx 托管，同时作为反向代理入口）。
FROM node:22-alpine AS builder

WORKDIR /src

COPY package.json package-lock.json* ./
RUN npm config set registry https://registry.npmmirror.com && npm install --no-audit --no-fund

COPY . .
RUN npm run build:only

FROM nginx:1.27-alpine

# 移除默认站点，使用平台自带配置
RUN rm -f /etc/nginx/conf.d/default.conf

COPY --from=builder /src/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/middleware-ops.conf

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1/healthz || exit 1

CMD ["nginx", "-g", "daemon off;"]
