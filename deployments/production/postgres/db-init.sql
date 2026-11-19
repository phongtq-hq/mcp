-- Tài khoản và database của máy chủ triển khai (ADR-0017). Chạy bằng psql sau `migrate up`
-- (các role app_api / app_gateway / app_worker do migration tạo), bằng tài khoản quản trị
-- của database. Chạy lại được: thứ đã có thì giữ, mật khẩu luôn được đặt lại theo biến,
-- nên đổi mật khẩu = sửa file env rồi chạy lại bước này.
--
-- Biến psql (truyền bằng -v): api_pw, gateway_pw, worker_pw, temporal_pw, keycloak_pw.
-- Mật khẩu đi qua format(%L): không nối chuỗi SQL bằng tay.
\set ON_ERROR_STOP on

-- Mỗi process của platform một tài khoản đăng nhập, là thành viên của đúng một role ứng
-- dụng. Process từ chối chạy bằng superuser hay role có BYPASSRLS.
SELECT format('CREATE ROLE %I LOGIN IN ROLE %I', l.login, l.app_role)
FROM (VALUES ('hq_api', 'app_api'), ('hq_gateway', 'app_gateway'), ('hq_worker', 'app_worker')) AS l(login, app_role)
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = l.login) \gexec

SELECT format('ALTER ROLE hq_api PASSWORD %L', :'api_pw') \gexec
SELECT format('ALTER ROLE hq_gateway PASSWORD %L', :'gateway_pw') \gexec
SELECT format('ALTER ROLE hq_worker PASSWORD %L', :'worker_pw') \gexec

-- Temporal và Keycloak đi kèm: mỗi thứ một tài khoản và database riêng trên cùng máy chủ
-- PostgreSQL. Chúng không có quyền gì trên database của platform. Bỏ trống mật khẩu
-- (không dùng thành phần đó) thì không tạo gì.
SELECT format('CREATE ROLE temporal LOGIN')
WHERE :'temporal_pw' <> '' AND NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'temporal') \gexec
SELECT format('ALTER ROLE temporal PASSWORD %L', :'temporal_pw') WHERE :'temporal_pw' <> '' \gexec
SELECT format('CREATE DATABASE %I OWNER temporal', d.name)
FROM (VALUES ('temporal'), ('temporal_visibility')) AS d(name)
WHERE :'temporal_pw' <> '' AND NOT EXISTS (SELECT FROM pg_database WHERE datname = d.name) \gexec

SELECT format('CREATE ROLE keycloak LOGIN')
WHERE :'keycloak_pw' <> '' AND NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'keycloak') \gexec
SELECT format('ALTER ROLE keycloak PASSWORD %L', :'keycloak_pw') WHERE :'keycloak_pw' <> '' \gexec
SELECT format('CREATE DATABASE keycloak OWNER keycloak')
WHERE :'keycloak_pw' <> '' AND NOT EXISTS (SELECT FROM pg_database WHERE datname = 'keycloak') \gexec

-- Database của platform không cho PUBLIC nối vào: chỉ các tài khoản ở trên (qua role ứng
-- dụng) và tài khoản quản trị.
SELECT format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO app_api, app_gateway, app_worker', current_database()) \gexec
