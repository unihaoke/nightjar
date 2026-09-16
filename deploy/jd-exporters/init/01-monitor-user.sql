-- jd 项目（面试演练系统）MySQL 监控账号初始化
--
-- 由 deploy/jd-exporters/docker-compose.jd.yml 挂载进 MySQL 容器执行，
-- 仅新增只读监控账号，不修改 jd 项目自身的表结构（原有 ddl-auto=update 负责建表）。
--
-- 使用方式：见 docs/COLLECTOR-JD.md 第 3.1 节。
-- 该文件只在「数据库首次初始化（空数据卷）」时由 MySQL 容器执行；
-- 若已有 mysql-data 卷，请用下方 SQL 手工创建（见文档第 3.2 节）。

-- 只读监控账号：mysqld-exporter 需要 PROCESS 与 REPLICATION CLIENT
CREATE USER IF NOT EXISTS 'exporter'@'%' IDENTIFIED WITH mysql_native_password BY 'exporter_change_me';
GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';
FLUSH PRIVILEGES;
