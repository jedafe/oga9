-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — staged
-- 'voice-rooms' app alongside comms_documents (chat).

INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('voice-rooms','1.0.0','Voice Rooms',false,'{
  "id":"voice-rooms","version":"1.0.0","requires":["comms_documents"],
  "permissions":["org.voice.start","org.voice.join"],
  "events":{"publishes":["VoiceRoomStarted","VoiceRoomEnded","VoiceParticipantJoined","VoiceParticipantLeft"],"consumes":[]},
  "entitlements":{"feature":"app.voice_rooms"}
}');
