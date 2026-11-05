# Changelog

## 0.2.0

- Leak detection: `binary_sensor.<name>_water_leak` turns on when EGL estimates a leak, when the meter's minimum flow of the day stays above a threshold for several days in a row (continuous flow: running toilet, dripping pipe...), or, optionally, on unusually high consumption. `sensor.<name>_water_min_flow` shows the latest day's minimum flow.
- New statistics for graphs: `<name> water minimum flow` (L/h) and `<name> water leak estimate` (L).
- Units declared by EGL are logged; `dump` keeps them.

## 0.1.0

- First release: daily water consumption and cost imported into Home Assistant statistics (Energy dashboard, Water).
