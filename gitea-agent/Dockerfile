FROM archlinux:base

RUN pacman -Syu --noconfirm --needed \
      nodejs-lts-jod \
      npm \
      yarn \
      git \
      ca-certificates \
      iptables \
      unzip \
      php \
      php-gd \
      php-intl \
      php-sodium \
      php-sqlite \
      composer \
    && sed -i -E 's/^;(extension=(bcmath|curl|exif|gd|iconv|intl|openssl|pdo_sqlite|sockets|sodium|sqlite3|zip))$/\1/' /etc/php/php.ini \
    && npm install -g @openai/codex \
    && rm -rf /var/cache/pacman/pkg/*
