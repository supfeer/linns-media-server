#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>

static NSString *localized_string(const char *value) {
    return [NSString stringWithUTF8String:value] ?: @"";
}

static char *choose_directories(const char *title, const char *prompt, const char *message) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
    [NSApp activateIgnoringOtherApps:YES];
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.canChooseFiles = NO;
    panel.canChooseDirectories = YES;
    panel.allowsMultipleSelection = YES;
    panel.resolvesAliases = YES;
    panel.title = localized_string(title);
    panel.prompt = localized_string(prompt);
    panel.message = localized_string(message);
    panel.directoryURL = [NSURL fileURLWithPath:NSHomeDirectory() isDirectory:YES];

    if ([panel runModal] != NSModalResponseOK) {
        return strdup("[]");
    }

    NSMutableArray<NSString *> *paths = [NSMutableArray arrayWithCapacity:panel.URLs.count];
    for (NSURL *url in panel.URLs) {
        if (url.fileURL) {
            [paths addObject:url.path];
        }
    }
    NSData *data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:nil];
    NSString *json = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
    return strdup(json.UTF8String ?: "[]");
}

char *folderdlna_choose_directories(const char *title, const char *prompt, const char *message) {
	if ([NSThread isMainThread]) {
		return choose_directories(title, prompt, message);
    }

	__block char *result = NULL;
	dispatch_sync(dispatch_get_main_queue(), ^{
		result = choose_directories(title, prompt, message);
    });
    return result;
}
