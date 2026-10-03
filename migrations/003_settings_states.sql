-- Добавление новых статусов в bot_state_enum
ALTER TYPE public.bot_state_enum ADD VALUE IF NOT EXISTS 'settings_menu';
ALTER TYPE public.bot_state_enum ADD VALUE IF NOT EXISTS 'edit_profile_name';
ALTER TYPE public.bot_state_enum ADD VALUE IF NOT EXISTS 'edit_profile_phone';
