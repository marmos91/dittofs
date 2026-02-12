-- Rollback: Remove NSM client registrations table

DROP INDEX IF EXISTS idx_nsm_client_registrations_mon_name;
DROP INDEX IF EXISTS idx_nsm_client_registrations_registered_at;
DROP INDEX IF EXISTS idx_nsm_client_registrations_callback_host;
DROP TABLE IF EXISTS nsm_client_registrations;
