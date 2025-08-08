# timetracking

go build -o timetrackcli timetrackcli.go


Change daily goal

./timetrackcli --config dailygoal=07:30


Working day

# Monday to Friday (default)
./timetrackcli --config workdays=Mon-Fri


# Wednesday to Friday only
./timetrackcli --config workdays=Wed-Fri

# Specific days (Monday, Tuesday, Wednesday, Friday)
./timetrackcli --config workdays=Mon,Tue,Wed,Fri