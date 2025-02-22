import java.text.DateFormat
import java.text.SimpleDateFormat
import java.util.TimeZone
import org.gradle.api.DefaultTask
import org.gradle.api.tasks.Input
import org.gradle.api.tasks.InputFiles
import org.gradle.api.tasks.Optional
import org.gradle.api.tasks.TaskAction
import org.gradle.process.ExecResult

class DockerCompose extends DefaultTask {

    @InputFiles
    List<File> composeFiles = []

    @Input
    @Optional
    String projectName

    @Input
    List<String> args = []

    private final DateFormat dateFormat = new SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss'Z'")
    private Date startTime

    DockerCompose() {
        dateFormat.timeZone = TimeZone.getTimeZone('UTC')
    }

    void composeFile(Object f) {
        composeFiles << project.file(f)
    }

    void projectName(String p) {
        projectName = p
    }

    void args(Object... args) {
        this.args += [*args]
    }

    void dumpLogs(String service = '') {
        List<String> cmd = [*dockerCommand(), 'logs']
        if (startTime != null) {
            cmd += ['--since', dateFormat.format(startTime)]
        }
        if (service != '') {
            cmd << service
        }
        project.exec { commandLine cmd }
    }

    @TaskAction
    void exec() {
        startTime = new Date()
        List<String> cmd = [*dockerCommand(), *args]
        ExecResult execResult = project.exec { commandLine cmd }
        if (execResult.exitValue != 0) {
            println "\nDocker command failed: '${cmd}'. Dumping Docker logs."
            dumpLogs()
        }
        execResult.assertNormalExitValue()
    }

    private List<String> dockerCommand() {
        String dockerComposeV2Check = 'docker compose version'.execute().text
        List<String> cmd = dockerComposeV2Check.contains('Docker Compose')
            ? ['docker', 'compose'] : ['docker-compose']
        composeFiles.each { f ->
            cmd += ['-f', f]
        }
        if (projectName != null) {
            cmd += ['-p', projectName]
        }
        return cmd
    }

}
