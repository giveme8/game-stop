extends Node
## Attach to a game's scene. Caller supplies ephemeral configuration in memory;
## do not export credentials into .tscn/.pck, read management tokens, or use autoload.
const SDK = preload("game_stop.gd")
var platform: Node
var pending_submission_id := ""
var pending_value := 0


func initialize_platform(base_url: String, game_id: String, play_token: String = "") -> Dictionary:
	if platform == null:
		platform = SDK.new()
		add_child(platform)
	return platform.configure(base_url, game_id, play_token)


func read_public_board() -> Dictionary:
	return await platform.get_leaderboard()


func finish_round(value: int) -> Dictionary:
	# Keep the ID on transport errors. Retry with the SAME value/board/ID; begin a
	# new logical round only after resolving or explicitly abandoning the old one.
	if pending_submission_id.is_empty():
		pending_submission_id = SDK.new_submission_id()
		pending_value = value
	if value != pending_value:
		return {"ok": false, "status": 0, "data": null, "error": "pending_round_value_changed"}
	var response: Dictionary = await platform.submit_score(pending_value, "score", pending_submission_id)
	if response.ok:
		pending_submission_id = ""
	return response


func read_aloud(text: String) -> bool:
	return platform.speak(text, "zh")
