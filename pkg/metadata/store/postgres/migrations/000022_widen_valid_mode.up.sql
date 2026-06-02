-- Widen the files.mode CHECK constraint to admit DittoFS high mode bits.
--
-- The SMB adapter stores DOS file attributes (ARCHIVE, SYSTEM, READONLY,
-- COMPRESSED, SPARSE, plus the "DOS attributes were explicitly set" marker) in
-- high bits of the Unix mode field rather than mutating POSIX permission bits.
-- See internal/adapter/smb/handlers/converters.go: modeDOSExplicit (0x10000),
-- modeDOSArchive (0x20000), modeDOSCompressed (0x40000), modeDOSSystem
-- (0x80000), modeDOSReadonly (0x100000), modeDOSSparse (0x200000).
--
-- The original valid_mode CHECK (mode <= 4095 / 0o7777) only admitted POSIX
-- permission bits, so a SET_INFO FILE_BASIC_INFORMATION that sets any DOS
-- attribute produced a mode such as 0x101A4 and tripped the constraint
-- (SQLSTATE 23514). mapPgError surfaces that as ErrInvalidArgument, which the
-- SMB layer maps to STATUS_INVALID_PARAMETER — the 4 WPTS BVT ChangeNotify
-- tests fail on postgres while passing on the memory/badger backends, which
-- store the full uint32 mode without any range check (#882).
--
-- The mode column is INTEGER (signed 31-bit positive range); the DOS bits top
-- out around 0x3F0FFF, well within it. Keep only the non-negative lower bound
-- so the postgres backend matches the memory/badger fidelity for the full mode
-- value DittoFS uses.

ALTER TABLE files DROP CONSTRAINT IF EXISTS valid_mode;
ALTER TABLE files ADD CONSTRAINT valid_mode CHECK (mode >= 0);
