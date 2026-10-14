package hass

// baselineQuestions is the household question set as a model answered it with
// the tools before results were shaped (no area, device_class or state filter;
// statistics by id only). Kept to show how those call patterns fare now.
func baselineQuestions() []replayQuestion {
	return []replayQuestion{
		{"Is anyone home?", []replayCall{
			{"get_home_states", map[string]any{"domain": "person"}},
		}},
		{"Which lights are on?", []replayCall{
			{"get_home_states", map[string]any{"domain": "light"}},
		}},
		{"Did the garage close after we left?", []replayCall{
			{"get_home_states", map[string]any{"search": "garage"}},
			{"get_state_history", map[string]any{"entity_id": "cover.garage_doors_left_garage_door,cover.garage_doors_right_garage_door,person.resident_a,person.resident_b,person.resident_c", "hours": 24}},
		}},
		{"Why was the heating on all night?", []replayCall{
			{"get_home_states", map[string]any{"search": "heat"}},
			{"get_state_history", map[string]any{"entity_id": "sensor.heat_pump_power_minute_average,sensor.electric_heater_power_minute_average", "hours": 24}},
		}},
		{"What's using the most power this week?", []replayCall{
			{"get_home_states", map[string]any{"search": "energy"}},
			{"get_long_term_statistics", map[string]any{"statistic_ids": "sensor.275_greentree_energy_today,sensor.hotwater_heater_energy_today,sensor.dryer_energy_today,sensor.heat_pump_energy_today,sensor.electric_heater_energy_today,sensor.well_pump_energy_today,sensor.dishwasher_energy_today,sensor.oven_energy_today,sensor.garage_plugs_energy_today,sensor.kitchen_plugs_energy_today,sensor.balance_energy_today", "hours": 168, "period": "day"}},
		}},
		{"How warm is the nursery?", []replayCall{
			{"get_home_states", map[string]any{"search": "temperature"}},
		}},
		{"Is the car charging?", []replayCall{
			{"get_home_states", map[string]any{"search": "charg"}},
		}},
		{"Any doors or windows open?", []replayCall{
			{"get_home_states", map[string]any{"domain": "binary_sensor,cover,lock"}},
		}},
		{"What's on the calendar this week?", []replayCall{
			{"get_calendar_events", map[string]any{}},
		}},
		{"What happened in the last hour?", []replayCall{
			{"get_home_events", map[string]any{"hours": 1}},
		}},
		{"What happened at home today?", []replayCall{
			{"get_home_events", map[string]any{"hours": 24}},
		}},
		{"What's on in the kitchen?", []replayCall{
			{"get_home_registry", map[string]any{"area_id": "kitchen"}},
			{"get_home_states", map[string]any{"search": "kitchen"}},
		}},
		{"Is Home Assistant healthy?", []replayCall{
			{"get_diagnostics", map[string]any{}},
		}},
		{"What time zone/version is HA on?", []replayCall{
			{"get_home_config", map[string]any{}},
		}},
		{"Overview: unfiltered states", []replayCall{
			{"get_home_states", map[string]any{}},
		}},
		{"Overview: unfiltered registry", []replayCall{
			{"get_home_registry", map[string]any{}},
		}},
	}
}

// liveQuestions is the same question set as a model answers it with the
// current tools, following their descriptions.
func liveQuestions() []replayQuestion {
	return []replayQuestion{
		{"Is anyone home?", []replayCall{
			{"get_home_states", map[string]any{"domain": "person"}},
		}},
		{"Which lights are on?", []replayCall{
			{"get_home_states", map[string]any{"domain": "light", "state": "on"}},
		}},
		{"Did the garage close after we left?", []replayCall{
			{"get_home_events", map[string]any{"domain": "cover,person", "hours": 24}},
		}},
		{"Why was the heating on all night?", []replayCall{
			{"get_home_states", map[string]any{"search": "heat", "device_class": "power"}},
			{"get_state_history", map[string]any{"entity_id": "sensor.heat_pump_power_minute_average,sensor.electric_heater_power_minute_average", "hours": 12}},
		}},
		{"What's using the most power this week?", []replayCall{
			{"get_long_term_statistics", map[string]any{"search": "energy_today", "hours": 168}},
		}},
		{"How warm is the nursery?", []replayCall{
			{"get_home_states", map[string]any{"search": "nursery", "device_class": "temperature"}},
		}},
		{"Is the car charging?", []replayCall{
			{"get_home_states", map[string]any{"search": "charg"}},
		}},
		{"Any doors or windows open?", []replayCall{
			{"get_home_states", map[string]any{"device_class": "door,window,garage_door,garage,opening", "state": "on,open"}},
		}},
		{"What's on the calendar this week?", []replayCall{
			{"get_calendar_events", map[string]any{}},
		}},
		{"What happened in the last hour?", []replayCall{
			{"get_home_events", map[string]any{"hours": 1}},
		}},
		{"What happened at home today?", []replayCall{
			{"get_home_events", map[string]any{"hours": 24}},
		}},
		{"What's on in the kitchen?", []replayCall{
			{"get_home_states", map[string]any{"area": "kitchen"}},
		}},
		{"Is Home Assistant healthy?", []replayCall{
			{"get_diagnostics", map[string]any{}},
		}},
		{"What time zone/version is HA on?", []replayCall{
			{"get_home_config", map[string]any{}},
		}},
		{"Overview: unfiltered states", []replayCall{
			{"get_home_states", map[string]any{}},
		}},
		{"Overview: unfiltered registry", []replayCall{
			{"get_home_registry", map[string]any{}},
		}},
	}
}
