#import <Foundation/Foundation.h>

// 0 = nominal, 1 = fair, 2 = serious, 3 = critical (NSProcessInfoThermalState).
int thermal_state(void) {
	@autoreleasepool {
		return (int)[[NSProcessInfo processInfo] thermalState];
	}
}

int low_power_mode(void) {
	@autoreleasepool {
		return [[NSProcessInfo processInfo] isLowPowerModeEnabled] ? 1 : 0;
	}
}
