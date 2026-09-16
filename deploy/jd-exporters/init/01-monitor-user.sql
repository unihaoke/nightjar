-- jd 项目（Java 面试复习工具）MySQL 只读监控账号初始化
--
-- 由 deploy/jd-exporters/docker-compose.jd.yml 挂载进 MySQL 容器执行，
-- 仅新增只读监控账号，不影响 jd 项目自身的表结构（原有 ddl-auto=update 负责建表）。
--
-- 该文件只在「数据库首次初始化（空数据卷）」时由 MySQL 容器执行；
-- 若已有 mysql-data 数据卷，用一次性容器补建：
--   docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml \
--     --profile initdb run --rm mysql-monitor-user
--
-- 上游来源：https://github.com/unihaoke/nightjar
--
-- 【必改】下面的口令必须同时改这两处，保持一致：
--   1) 本文件的 BY '<口令>'
--   2) jd 根目录 .env 的 MYSQL_EXPORTER_PASSWORD
--      （mysqld-exporter 走 DATA_SOURCE_NAME 注入，见 docker-compose.jd.yml）
-- 口令含 @ ( ) / 等特殊字符时，DATA_SOURCE_NAME 会解析失败，
-- 此时改用 my.cnf 挂载方式（见 deploy/jd-exporters/my.cnf 头部注释）。
--
-- 注意：本文件只在「空数据卷首次初始化」时自动执行；已有数据卷请用 initdb 容器补建。

-- mysqld-exporter 需要 PROCESS 与 REPLICATION CLIENT；MySQL 8.0 要求 mysql_native_password
CREATE USER IF NOT EXISTS 'exporter'@'%' IDENTIFIED WITH mysql_native_password BY 'exporter_change_me';
GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';
FLUSH PRIVILEGES;
